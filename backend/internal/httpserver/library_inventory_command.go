package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type inventoryQuery struct {
	ID, Kind, Title, Year, Category, OriginalTitle, IMDb string
	SeasonEpisode                                        string
	TVDetail                                             json.RawMessage
	LocalOnly                                            bool
	Naming                                               map[string]string
	Metadata                                             mediameta.Metadata
	Totals                                               map[int]int
}

type missingLibrarySeason struct {
	Season   int   `json:"season"`
	Episodes []int `json:"episodes"`
	Total    int   `json:"total_episodes"`
}

type libraryCheck struct {
	Complete bool                   `json:"complete"`
	Missing  []missingLibrarySeason `json:"missing"`
	Messages []string               `json:"messages"`
}

// The old scheduler, chat receiver and RSS reader run on the machine with the
// configuration volume. Each invokes this command against that volume.
func LibraryInventoryCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var q inventoryQuery
	if readMediaLookup(in, &q) != nil || !(q.LocalOnly && q.ID == "" || numericTMDBID(q.ID)) || strings.TrimSpace(q.Title) == "" ||
		(q.Kind != "电影" && q.Kind != "电视剧" && q.Kind != "动漫") {
		return errors.New("invalid library lookup")
	}
	ctx, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	info, err := os.Stat(dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("library settings unavailable")
	}
	system, err := systemconfig.Open(dbPath)
	if err != nil {
		return err
	}
	defer system.Close()
	service := subscriptionService{systemConfig: system, configStore: config.NewStore(path), client: http.DefaultClient}
	result, err := service.checkLibrarySelection(ctx, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func (service subscriptionService) checkLibrarySelection(ctx context.Context, q inventoryQuery) (libraryCheck, error) {
	var empty libraryCheck
	id, err := strconv.ParseInt(q.ID, 10, 64)
	if !(q.LocalOnly && q.ID == "") && (err != nil || id <= 0) {
		return empty, mediaserver.ErrConfiguration
	}
	kind := "movie"
	detail := tmdbMediaDetails{ID: id, Title: q.Title, Attributes: map[string]any{"original_title": q.OriginalTitle, "imdb_id": q.IMDb}}
	if q.Kind != "电影" {
		kind = "tv"
	}
	if kind == "tv" && !q.LocalOnly {
		if len(q.TVDetail) > 0 {
			detail, err = decodeTMDBDetails(q.TVDetail, q.ID)
		} else {
			settings, settingsErr := service.configStore.Snapshot()
			if settingsErr != nil {
				return empty, settingsErr
			}
			rt := service.client.Transport
			proxies := objectValue(objectValue(settings["app"])["proxies"])
			if text(proxies["https"]) != "" {
				t, err := siteProxyTransport(rt, proxies, "https")
				if err != nil {
					return empty, err
				}
				defer (&http.Client{Transport: t}).CloseIdleConnections()
				rt = t
			}
			detail, err = fetchNativeTMDBDetails(ctx, service.configStore, rt, kind, q.ID)
		}
		if err != nil {
			return empty, err
		}
		if strings.TrimSpace(detail.Name) == "" || detail.Attributes["media_type"] != nil && detail.Attributes["media_type"] != "tv" {
			return empty, mediaserver.ErrResponse
		}
		// The selected title can be a localized alias. The organized library
		// and the receiver both use that same title and year.
		detail.Name = q.Title
		detail.FirstAirDate = q.Year
	}
	if kind == "tv" {
		if q.LocalOnly || q.OriginalTitle != "" {
			detail.Attributes["original_name"] = q.OriginalTitle
		}
		if q.IMDb != "" {
			detail.Attributes["imdb_id"] = q.IMDb
		}
	}
	detail.inventoryNames = q.Naming
	q.Metadata.Year = q.Year
	if q.LocalOnly {
		settings, err := service.configStore.Snapshot()
		if err != nil {
			return empty, err
		}
		if q.Kind == "动漫" {
			detail.Attributes["genres"] = []any{map[string]any{"id": 16}}
		}
		identity := mediaserver.Identity{Title: q.Title, Year: q.Year, TMDBID: q.ID, TV: kind == "tv"}
		if kind == "tv" && (q.Metadata.Episodes.Season == nil || len(q.Totals) != 1) {
			return empty, mediaserver.ErrConfiguration
		}
		inv, err := service.nativeLocalMediaInventory(ctx, objectValue(settings["media"]), q.Metadata, detail, identity, []string{q.Category})
		if err != nil {
			return empty, err
		}
		return describeLibraryInventory(q, detail, inv)
	}
	inventory, err := service.nativeMediaInventory(ctx, q.Metadata, detail, kind, q.Category)
	if err != nil {
		return empty, err
	}
	return describeLibraryInventory(q, detail, inventory)
}

func describeLibraryInventory(q inventoryQuery, detail tmdbMediaDetails, inventory mediaserver.Inventory) (libraryCheck, error) {
	result := libraryCheck{Missing: []missingLibrarySeason{}, Messages: []string{}}
	label := q.Title
	if q.Year != "" {
		label += " (" + q.Year + ")"
	}
	if q.Kind == "电影" {
		result.Complete = len(inventory.ItemIDs) > 0
		if result.Complete {
			result.Messages = append(result.Messages, "媒体库中已存在电影：\n • "+label)
		}
		return result, nil
	}
	bounds := q.Metadata.Episodes
	seasons := []int{}
	totals := map[int]int{}
	for _, s := range detail.Seasons {
		if _, dup := totals[s.Number]; dup {
			return result, mediaserver.ErrResponse
		}
		totals[s.Number] = 0
		if s.Episodes != nil {
			totals[s.Number] = *s.Episodes
		}
		if s.Number > 0 && (s.Episodes == nil || *s.Episodes != 0 || q.Totals[s.Number] > 0) {
			seasons = append(seasons, s.Number)
		}
	}
	explicit := bounds.Season != nil || bounds.Episode != nil
	if explicit {
		begin, end := 1, 1
		if bounds.Season != nil {
			begin, end = *bounds.Season, *bounds.Season
		}
		if bounds.EndSeason != nil {
			end = *bounds.EndSeason
		}
		if begin < 0 || end < begin || end > 10000 || end-begin >= 1000 {
			return result, mediaserver.ErrConfiguration
		}
		seasons = nil
		for n := begin; n <= end; n++ {
			seasons = append(seasons, n)
		}
	} else {
		slices.Reverse(seasons)
		result.Messages = append(result.Messages, fmt.Sprintf("%s %s 共有 %d 季", q.Kind, label, len(seasons)))
	}
	for season, total := range q.Totals {
		if total == 0 {
			continue
		}
		if season < 0 || total < 0 || total > 10000 {
			return result, mediaserver.ErrConfiguration
		}
		totals[season] = total
	}
	if len(seasons) == 0 {
		return result, mediaserver.ErrUnknownCoverage
	}
	full, err := inventory.Coverage(mediaserver.CoverageSelection{TV: true, Seasons: seasons, SeasonTotals: totals})
	if err != nil {
		return result, err
	}
	result.Complete = full.Complete
	if bounds.Episode != nil {
		begin, end := *bounds.Episode, *bounds.Episode
		if bounds.EndEpisode != nil {
			end = *bounds.EndEpisode
		}
		if begin < 1 || end < begin || end > 10000 {
			return result, mediaserver.ErrConfiguration
		}
		result.Complete = true
		for _, n := range seasons {
			if end > totals[n] {
				return result, mediaserver.ErrUnknownCoverage
			}
			for _, e := range full.Missing[n] {
				if e >= begin && e <= end {
					result.Complete = false
					break
				}
			}
		}
	}
	for _, n := range seasons {
		ep, total := full.Missing[n], totals[n]
		prefix := fmt.Sprintf("第%d季 ", n)
		if explicit {
			prefix = q.Title + " " + prefix
		}
		msg := fmt.Sprintf("共%d集 已全部存在", total)
		if len(ep) > 0 {
			item := missingLibrarySeason{n, ep, total}
			if len(ep) == total {
				item.Episodes = []int{}
				msg = fmt.Sprintf("缺失 %d 集", total)
			} else {
				nums := make([]string, len(ep))
				for i, v := range ep {
					nums[i] = strconv.Itoa(v)
				}
				msg = "缺失集：" + strings.Join(nums, "、")
			}
			result.Missing = append(result.Missing, item)
		}
		result.Messages = append(result.Messages, prefix+msg)
	}
	if bounds.Episode != nil && result.Complete && !full.Complete {
		result.Messages = append(result.Messages, "媒体库中已存在剧集：\n • "+strings.TrimSpace(label+" "+q.SeasonEpisode))
	}
	return result, nil
}
