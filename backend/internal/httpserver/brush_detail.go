package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/antchfx/xmlquery"
)

type brushDetailRequest struct {
	Selection                          *brushSelection
	TaskID, SiteID                     int64
	FeedURL, Title, PageURL, Enclosure string
}

type brushAttributes struct {
	Decision   string `json:"decision,omitempty"`
	Free       *bool  `json:"free"`
	DoubleFree bool   `json:"2xfree"`
	HitAndRun  *bool  `json:"hr"`
	Seeders    *int64 `json:"peer_count"`
	Deadline   string `json:"free_deadline"`
}

var errBrushDetail = errors.New("brush task or detail unavailable")

// Only a running task can use its site's saved credentials. A task may have
// a custom feed, but its feed cannot choose another origin for detail cookies.
func (q brushDetailRequest) site(ctx context.Context, db *sql.DB, sites *siteconfig.Store) (siteconfig.Site, error) {
	var siteID int64
	var feed string
	err := db.QueryRowContext(ctx, `SELECT SITE,COALESCE(RSSURL,'') FROM SITE_BRUSH_TASK WHERE ID=? AND STATE='Y'`, q.TaskID).Scan(&siteID, &feed)
	if err != nil || siteID != q.SiteID {
		return siteconfig.Site{}, errBrushDetail
	}
	site, err := sites.Get(ctx, siteID)
	if err != nil || !strings.Contains(site.Include, "S") || site.Cookie == "" || site.RSSURL == "" || defaultString(feed, site.RSSURL) != q.FeedURL {
		return siteconfig.Site{}, errBrushDetail
	}
	return site, nil
}

func BrushDetailCommand(ctx context.Context, path, catalogPath string, in io.Reader, out io.Writer) error {
	var q brushDetailRequest
	if readMediaLookup(in, &q) != nil || q.TaskID <= 0 || q.SiteID <= 0 || strings.TrimSpace(q.Title) == "" || q.Enclosure == "" || q.FeedURL == "" {
		return errBrushDetail
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	service := subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	sites, err := siteconfig.Open(service.databasePath)
	if err != nil {
		return err
	}
	defer sites.Close()
	site, err := q.site(ctx, db, sites)
	if err != nil {
		return err
	}
	result := brushAttributes{}
	if q.Selection != nil {
		var hasReceipts, exists bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_BRUSH_SUBMISSION'),
			EXISTS(SELECT 1 FROM SITE_BRUSH_TORRENTS WHERE ENCLOSURE=?)`, q.Enclosure).Scan(&hasReceipts, &exists)
		if err == nil && hasReceipts && !exists {
			err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_BRUSH_SUBMISSION WHERE enclosure=?)`, q.Enclosure).Scan(&exists)
		}
		if err != nil {
			return err
		}
		if exists {
			result.Decision = "handled"
			return json.NewEncoder(out).Encode(result)
		}
	}
	if q.PageURL != "" {
		note, _, _, err := subscriptionRSSSiteNote(site)
		if err != nil {
			return err
		}
		reader := siteBrowser{sites: sites, config: config.NewStore(path), renderer: browserruntime.New(defaultString(os.Getenv("NASTOOL_BROWSER_BINARY"), "auto"))}
		var catalog rssDetailCatalog
		resource, err := catalog.readClassic(ctx, reader, catalogPath, site, note, siteFeedArticle{Title: q.Title, Link: &q.PageURL, Enclosure: q.Enclosure}, brushRSSDetail)
		if err != nil {
			return err
		}
		result = brushAttributes{Free: resource.Freeleech, DoubleFree: resource.Freeleech != nil && *resource.Freeleech && resource.UploadFactor != nil && *resource.UploadFactor == 2, HitAndRun: resource.HitAndRun, Seeders: resource.Seeders, Deadline: resource.PromotionEnd}
		if current, err := q.site(ctx, db, sites); err != nil || current != site {
			return errBrushDetail
		}
	}
	if q.Selection != nil {
		result.Decision = q.Selection.match(result, time.Now(), func() bool {
			delay, err := sites.AdmitRequest(ctx, site.ID, time.Now)
			return err == nil && delay == 0
		})
	}
	return json.NewEncoder(out).Encode(result)
}

var brushDuration = regexp.MustCompile(`^(?:([0-9]+)\s*(?:天|日)\s*)?(?:([0-9]+)\s*(?:小时|小時|时|時)\s*)?(?:([0-9]+)\s*(?:分钟|分鐘|分)\s*)?$`)

func brushDeadlineValue(raw string, now time.Time) string {
	raw = strings.Trim(strings.TrimSpace(raw), "()（） ")
	if value, err := time.ParseInLocation(time.DateTime, raw, now.Location()); err == nil {
		return value.Format("20060102_1504")
	}
	for _, prefix := range []string{"剩余时间", "剩餘時間", "剩余", "剩餘", "限时", "限時"} {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	}
	raw = strings.Trim(raw, " :：()（）")
	parts := brushDuration.FindStringSubmatch(raw)
	if parts == nil {
		return ""
	}
	var duration time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if parts[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil || n > int64((math.MaxInt64-duration)/unit) {
			return ""
		}
		duration += time.Duration(n) * unit
	}
	if duration == 0 {
		return ""
	}
	return now.Add(duration).Format("20060102_1504")
}

func brushHTMLDeadline(root *xmlquery.Node, match func(string) ([]*xmlquery.Node, error), now time.Time) (string, error) {
	nodes, err := match("FREE_DDL")
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		// Older NexusPHP sites put the remaining duration in the first heading badge.
		node, err := xmlquery.Query(root, "(//h1[@id='top']//span)[1]")
		if err != nil || node == nil {
			return "", err
		}
		return brushDeadlineValue(node.InnerText(), now), nil
	}
	var deadline string
	for _, node := range nodes {
		raw := defaultString(strings.TrimSpace(node.SelectAttr("title")), node.InnerText())
		value := brushDeadlineValue(raw, now)
		if value == "" {
			continue
		}
		if deadline != "" && value != deadline {
			return "", errBrushDetail
		}
		deadline = value
	}
	return deadline, nil
}
