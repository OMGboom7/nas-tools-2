package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/douban"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func readMediaLookup(in io.Reader, q any) error {
	input := &io.LimitedReader{R: in, N: 64 << 10}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 {
		return errors.New("invalid media lookup")
	}
	return nil
}

func sourceSubjectCommand(ctx context.Context, in io.Reader, out io.Writer) error {
	var q struct{ Provider, Kind, ID string }
	if readMediaLookup(in, &q) != nil || !numericTMDBID(q.ID) {
		return errors.New("invalid source selector")
	}
	var raw json.RawMessage
	var err error
	switch q.Provider {
	case "douban":
		raw, err = (douban.Client{}).SubjectDocument(ctx, q.Kind, q.ID)
	case "bangumi":
		if q.Kind != "tv" {
			return errors.New("invalid Bangumi kind")
		}
		raw, err = (subscriptionService{client: http.DefaultClient}).bangumiSubjectDocument(ctx, q.ID)
	default:
		return errors.New("unknown subject provider")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(raw)
}

type titleMatchQuery struct {
	Title, Kind, FirstYear, SeasonYear, Mode string
	Season                                   int
	Relax                                    bool
}

func titleMatchCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var q titleMatchQuery
	if readMediaLookup(in, &q) != nil || strings.TrimSpace(q.Title) == "" || (q.Kind != "movie" && q.Kind != "tv" && q.Kind != "multi") {
		return errors.New("invalid title match")
	}
	snapshot, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	client := &http.Client{}
	proxies := objectValue(objectValue(snapshot["app"])["proxies"])
	if text(proxies["https"]) != "" {
		client.Transport, err = siteProxyTransport(nil, proxies, "https")
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result, err := matchMediaTitle(ctx, client, snapshot, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

// The installed clients strip these separators but keep other symbols that
// distinguish titles. Only an English name starting with a letter may match a substring.
func legacyMatchName(s string) string {
	return cases.Upper(language.Und).String(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("、.。,，·:：;；!！'’\"“”()（）[]【】「」-—+|\\_/&#～~\u200b\u200c\u200d\ufeff", r) {
			return -1
		}
		return r
	}, s))
}

func matchMediaTitle(ctx context.Context, client *http.Client, settings map[string]any, q titleMatchQuery) (map[string]any, error) {
	type search struct {
		kind, year string
		bySeason   bool
	}
	if q.Mode != "" && q.Mode != "title" && q.Mode != "file" {
		return nil, errors.New("unknown recognition mode")
	}
	first := q.Kind
	if q.Mode == "title" && first != "tv" && q.FirstYear == "" {
		first = "multi"
	}
	steps := []search{}
	if q.Kind == "tv" && q.Season > 0 && q.SeasonYear != "" {
		steps = append(steps, search{"tv", "", true})
	}
	steps = append(steps, search{first, q.FirstYear, false})
	if first == "movie" && q.FirstYear != "" {
		y, err := strconv.Atoi(q.FirstYear)
		if err != nil || len(q.FirstYear) != 4 {
			return nil, errors.New("invalid film year")
		}
		steps = append(steps, search{"movie", strconv.Itoa(y + 1), false}, search{"movie", strconv.Itoa(y - 1), false})
		if q.Mode == "title" {
			steps = append(steps, search{"tv", q.FirstYear, false})
		}
	}
	if q.Relax && q.Mode != "" && q.FirstYear != "" {
		broad := first
		if first == "movie" && q.Mode == "title" {
			broad = "multi"
		}
		steps = append(steps, search{broad, "", false})
	}
	var incomplete error
	for _, step := range steps {
		q.Kind = step.kind
		row, err := matchTitlePage(ctx, client, settings, q, step.year, step.bySeason)
		if row != nil || ctx.Err() != nil {
			return row, ctx.Err()
		}
		if err != nil && incomplete == nil {
			incomplete = err
		}
	}
	return nil, incomplete
}

func titleYearMatches(date, year string) bool {
	return len(date) >= 4 && date[:4] == year
}

func matchTitlePage(ctx context.Context, client *http.Client, settings map[string]any, q titleMatchQuery, year string, bySeason bool) (map[string]any, error) {
	lang := "en"
	if strings.ContainsFunc(q.Title, func(r rune) bool { return r >= '一' && r <= '鿿' }) {
		lang = "zh"
	}
	filters := map[string]string{"language": lang, "query": q.Title}
	if year != "" {
		key := "year"
		if q.Kind == "tv" {
			key = "first_air_date_year"
		}
		filters[key] = year
	}
	page, err := fetchTMDBResultPage[json.RawMessage](ctx, client, settings, "/search/"+q.Kind, 1, filters)
	if err != nil {
		return nil, err
	}
	want := legacyMatchName(q.Title)
	if want == "" {
		return nil, nil
	}
	partial := want[0] >= 'A' && want[0] <= 'Z' && strings.Trim(want, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@") == ""
	matches := func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return false
		}
		name := legacyMatchName(s)
		return want == name || partial && strings.Contains(name, want)
	}
	var direct map[string]any
	var aliases []tmdbDetailQuery
	for index, raw := range page.Results {
		var row map[string]any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&row) != nil {
			return nil, errors.New("invalid TMDB candidate")
		}
		kind := q.Kind
		if kind == "multi" {
			kind = text(row["media_type"])
		}
		id := text(row["id"])
		if kind != "movie" && kind != "tv" || !numericTMDBID(id) {
			continue
		}
		title, original, date := "title", "original_title", "release_date"
		if kind == "tv" {
			title, original, date = "name", "original_name", "first_air_date"
		}
		premiere := text(row[date])
		if year != "" && !titleYearMatches(premiere, year) {
			continue
		}
		if (!bySeason || titleYearMatches(premiere, q.SeasonYear)) && (matches(row[title]) || matches(row[original])) {
			row["media_type"] = kind
			if q.Kind != "multi" {
				return row, nil
			}
			direct = row // The historical mixed list uses its last matching result.
		}
		if bySeason || q.Kind == "multi" {
			if index >= 5 {
				continue
			}
		} else if len(aliases) >= 6 {
			continue
		}
		aliases = append(aliases, tmdbDetailQuery{Kind: kind, ID: id, Localize: true})
	}
	if direct != nil {
		return direct, nil
	}
	var last map[string]any
	var incomplete error
	for _, candidate := range aliases {
		detail, err := fullTMDBDetail(ctx, settings, client.Transport, candidate)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if incomplete == nil {
				incomplete = err
			}
			continue
		}
		if !titleAliasMatches(detail, candidate.Kind, matches) {
			continue
		}
		if bySeason {
			found := false
			for _, s := range slice(detail["seasons"]) {
				season := objectValue(s)
				if text(season["season_number"]) == strconv.Itoa(q.Season) && titleYearMatches(text(season["air_date"]), q.SeasonYear) {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if q.Kind != "multi" {
			return detail, nil
		}
		last = detail
	}
	if last != nil {
		return last, nil
	}
	return nil, incomplete
}

func titleAliasMatches(detail map[string]any, kind string, matches func(any) bool) bool {
	key, field := "titles", "title"
	if kind == "tv" {
		key, field = "results", "name"
	}
	for _, a := range slice(objectValue(detail["alternative_titles"])[key]) {
		if matches(objectValue(a)["title"]) {
			return true
		}
	}
	for _, t := range slice(objectValue(detail["translations"])["translations"]) {
		if matches(objectValue(objectValue(t)["data"])[field]) {
			return true
		}
	}
	return false
}
