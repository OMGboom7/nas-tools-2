package builtinindexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

type torrentLeechRow struct {
	ID                                                         json.RawMessage `json:"fid"`
	Name, Filename                                             string
	Size, Seeders, Leechers, Completed, AddedTimestamp, IMDbID json.RawMessage
	Multiplier                                                 json.RawMessage `json:"download_multiplier"`
}

type torrentLeechItem struct {
	resource externalindexer.Resource
	source   torrentLeechRow
}

func (item torrentLeechItem) clientRow() map[string]any {
	r, s := item.resource, item.source
	return map[string]any{
		"indexer": r.IndexerID, "title": r.Title, "enclosure": r.DownloadURL, "page_url": r.PageURL,
		"pubdate": tnodeDate(s.AddedTimestamp), "size": s.Size, "seeders": s.Seeders, "peers": s.Leechers,
		"grabs": s.Completed, "downloadvolumefactor": s.Multiplier, "uploadvolumefactor": 1, "imdbid": s.IMDbID,
	}
}

func torrentLeechPage(ctx context.Context, d indexercatalog.Definition, cookie, ua, keyword string, page int, transport http.RoundTripper) ([]torrentLeechItem, error) {
	base, err := url.Parse(d.Domain)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		d.Parser != "TorrentLeech" || d.ID == "" || cookie == "" || strings.ContainsAny(cookie+ua, "\r\n") || page < 0 || page > 10000 ||
		len(keyword) > 1024 || strings.ContainsFunc(keyword, unicode.IsControl) || keyword == "." || keyword == ".." {
		return nil, ErrConfig
	}
	u := base.JoinPath("torrents/browse/list")
	if keyword != "" {
		u = u.JoinPath("query", url.PathEscape(keyword))
	}
	if keyword == "" || page > 0 {
		u = u.JoinPath("page", strconv.Itoa(page+1))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrConfig
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, err := tnodeResponse(ctx, client, req)
	if err != nil {
		return nil, err
	}
	var reply struct {
		Page        json.RawMessage
		TorrentList []torrentLeechRow
	}
	if json.Unmarshal(body, &reply) != nil || reply.TorrentList == nil || len(reply.TorrentList) > 2000 {
		return nil, ErrResponse
	}
	if len(reply.Page) > 0 {
		n, err := mteamInteger(reply.Page, true)
		if err != nil || *n != int64(page+1) {
			return nil, ErrResponse
		}
	}
	items := make([]torrentLeechItem, 0, len(reply.TorrentList))
	for _, row := range reply.TorrentList {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, err := mteamInteger(row.ID, true)
		if err != nil || *id < 1 || strings.TrimSpace(row.Name) == "" || len(row.Name) > 4096 || row.Filename == "" || len(row.Filename) > 4096 || strings.ContainsAny(row.Filename, "/\\") || strings.ContainsFunc(row.Filename, unicode.IsControl) || row.Filename == "." || row.Filename == ".." {
			return nil, ErrResponse
		}
		fid := strconv.FormatInt(*id, 10)
		res := externalindexer.Resource{IndexerID: d.ID, Indexer: d.Name, Title: row.Name, DownloadURL: base.JoinPath("download", fid, url.PathEscape(row.Filename)).String(), PageURL: base.JoinPath("torrent", fid).String()}
		size, err := mteamInteger(row.Size, true)
		if err != nil {
			return nil, err
		}
		res.Size = *size
		res.Seeders, err = mteamInteger(row.Seeders, false)
		if err != nil {
			return nil, err
		}
		res.Peers, err = mteamInteger(row.Leechers, false)
		if err != nil {
			return nil, err
		}
		if _, err := mteamInteger(row.Completed, false); err != nil {
			return nil, err
		}
		res.DownloadFactor, err = tnodeFactor(row.Multiplier)
		if err != nil {
			return nil, err
		}
		if res.DownloadFactor != nil {
			free := *res.DownloadFactor == 0
			res.Freeleech = &free
		}
		one := 1.0
		res.UploadFactor = &one
		if len(row.IMDbID) > 0 && string(row.IMDbID) != "null" {
			value, err := scalar(row.IMDbID)
			if err != nil {
				return nil, ErrResponse
			}
			if imdbPattern.MatchString(value) {
				res.IMDbID = value
			} else if imdbPattern.MatchString("tt" + value) {
				res.IMDbID = "tt" + value
			}
		}
		items = append(items, torrentLeechItem{res, row})
	}
	return items, nil
}

func SearchTorrentLeech(ctx context.Context, d indexercatalog.Definition, cookie, ua, keyword string, page int, transport http.RoundTripper) ([]externalindexer.Resource, error) {
	items, err := torrentLeechPage(ctx, d, cookie, ua, keyword, page, transport)
	if err != nil {
		return nil, err
	}
	out := make([]externalindexer.Resource, len(items))
	for i, item := range items {
		out[i] = item.resource
	}
	return out, nil
}
