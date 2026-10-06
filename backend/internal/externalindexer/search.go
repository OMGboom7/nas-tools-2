package externalindexer

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Download URLs can carry tracker credentials. They remain server-side and
// must not be included in client-facing errors or used as resource IDs.
type Resource struct {
	IndexerID, Indexer, Title, Description, PageURL, IMDbID string
	DownloadURL                                             string `json:"-"`
	DownloadResolver                                        string `json:"-"`
	Size                                                    int64
	Seeders                                                 *int64
	Peers                                                   *int64
	DownloadFactor, UploadFactor                            *float64
	Freeleech                                               *bool
	// Nil is unknown; explicit zero is a declared zero requirement.
	MinimumSeedTime *int64
	MinimumRatio    *float64
}

func validRemoteID(kind, id string) bool {
	if !validText(id, 128) || id == "." || id == ".." {
		return false
	}
	if kind == "Prowlarr" {
		n, err := strconv.ParseInt(id, 10, 64)
		return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// Search constructs its endpoint from configured host and verified remote ID;
// it never executes an arbitrary Indexer.Domain supplied by a caller.
func Search(ctx context.Context, cfg Config, indexer Indexer, keyword string, transport http.RoundTripper) ([]Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := baseURL(cfg)
	if err != nil {
		return nil, err
	}
	if indexer.Kind != cfg.Kind || !validRemoteID(cfg.Kind, indexer.RemoteID) || !validText(strings.TrimSpace(keyword), 1024) {
		return nil, ErrConfig
	}
	query := url.Values{"type": {"search"}, "query": {keyword}, "indexerIds": {indexer.RemoteID}, "limit": {"100"}, "offset": {"0"}}
	u := endpoint(base, "/api/v1/search")
	if cfg.Kind == "Jackett" {
		u = endpoint(base, "/api/v2.0/indexers/"+indexer.RemoteID+"/results/torznab/")
		// Jackett ResultsController.RequiresApiKey reads query parameters, not
		// X-Api-Key: https://github.com/Jackett/Jackett/blob/master/src/Jackett.Server/Controllers/ResultsController.cs
		query = url.Values{"apikey": {cfg.APIKey}, "t": {"search"}, "q": {keyword}}
	}
	u.RawQuery = query.Encode()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrConfig
	}
	if cfg.Kind == "Prowlarr" {
		r.Header.Set("X-Api-Key", cfg.APIKey)
	}
	r.Header.Set("Accept", "application/json, application/rss+xml")
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrResponse
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse || !utf8.Valid(body) {
		return nil, ErrResponse
	}
	if cfg.Kind == "Jackett" {
		return parseTorznab(ctx, body, indexer)
	}
	return parseProwlarr(ctx, body, indexer)
}

func validDownloadURL(raw string) bool {
	if !validText(raw, 16<<10) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	if u.Scheme == "magnet" {
		for _, xt := range u.Query()["xt"] {
			if hash, ok := strings.CutPrefix(xt, "urn:btih:"); ok {
				if len(hash) == 40 {
					if _, err := hex.DecodeString(hash); err == nil {
						return true
					}
				}
				if len(hash) == 32 {
					if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(hash)); err == nil {
						return true
					}
				}
			}
			if hash, ok := strings.CutPrefix(xt, "urn:btmh:1220"); ok && len(hash) == 64 {
				if _, err := hex.DecodeString(hash); err == nil {
					return true
				}
			}
		}
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.Fragment == ""
}

// ValidDownloadURL shares the download-address checks with native builtin
// indexers. Callers still enforce the configured tracker origin for HTTP URLs.
func ValidDownloadURL(raw string) bool { return validDownloadURL(raw) }

func parseProwlarr(ctx context.Context, body []byte, indexer Indexer) ([]Resource, error) {
	var rows []struct {
		ID          int64  `json:"indexerId"`
		Indexer     string `json:"indexer"`
		Title       string `json:"title"`
		Download    string `json:"downloadUrl"`
		Description string `json:"sortTitle"`
		Size        *int64 `json:"size"`
		Seeders     *int64 `json:"seeders"`
		Guid        string `json:"guid"`
	}
	if json.Unmarshal(body, &rows) != nil || rows == nil || len(rows) > 1000 {
		return nil, ErrResponse
	}
	items := []Resource{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strconv.FormatInt(row.ID, 10) != indexer.RemoteID || row.Size == nil || *row.Size < 0 || row.Seeders != nil && *row.Seeders < 0 {
			return nil, ErrResponse
		}
		if !validText(row.Title, 4096) || !validDownloadURL(row.Download) {
			continue
		}
		items = append(items, Resource{IndexerID: indexer.ID, Indexer: indexer.Name, Title: row.Title, DownloadURL: row.Download, Description: row.Description, Size: *row.Size, Seeders: row.Seeders, PageURL: row.Guid})
	}
	return items, nil
}

type torznabItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Size        string `xml:"size"`
	Comments    string `xml:"comments"`
	Enclosure   struct {
		URL    string `xml:"url,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
	Attributes []struct {
		XMLName xml.Name
		Name    string `xml:"name,attr"`
		Value   string `xml:"value,attr"`
	} `xml:"attr"`
}

func nonnegativeInteger(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, ErrResponse
	}
	return n, nil
}

func parseTorznab(ctx context.Context, body []byte, indexer Indexer) ([]Resource, error) {
	// Validate the entire bounded document before decoding items. Disallow
	// DTD directives and bound nesting, including inside otherwise ignored tags.
	d := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	root := ""
	roots := 0
	channels := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrResponse
		}
		switch node := token.(type) {
		case xml.StartElement:
			if depth == 1 && node.Name.Local == "channel" {
				channels++
			}
			if depth == 0 {
				roots++
				root = node.Name.Local
			}
			depth++
			if depth > 64 {
				return nil, ErrResponse
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return nil, ErrResponse
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(node)) != "" {
				return nil, ErrResponse
			}
		}
	}
	if root != "rss" || roots != 1 || depth != 0 || channels != 1 {
		return nil, ErrResponse
	}
	d = xml.NewDecoder(bytes.NewReader(body))
	items := []Resource{}
	path := []string{}
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrResponse
		}
		switch node := token.(type) {
		case xml.StartElement:
			if node.Name.Local != "item" || len(path) != 2 || path[0] != "rss" || path[1] != "channel" {
				path = append(path, node.Name.Local)
				continue
			}
			count++
			if count > 1000 {
				return nil, ErrResponse
			}
			var row torznabItem
			if d.DecodeElement(&row, &node) != nil {
				return nil, ErrResponse
			}
			if !validText(row.Title, 4096) || !validDownloadURL(row.Enclosure.URL) {
				continue
			}
			size := row.Size
			if size == "" {
				size = row.Enclosure.Length
			}
			amount, err := nonnegativeInteger(size)
			if err != nil {
				return nil, err
			}
			peers := int64(0)
			seeders := int64(0)
			download, upload := 1.0, 1.0
			free := false
			item := Resource{IndexerID: indexer.ID, Indexer: indexer.Name, Title: row.Title, DownloadURL: row.Enclosure.URL, Description: row.Description, PageURL: row.Comments, Size: amount, Seeders: &seeders, Peers: &peers, DownloadFactor: &download, UploadFactor: &upload, Freeleech: &free}
			seen := map[string]bool{}
			for _, attribute := range row.Attributes {
				if attribute.XMLName.Space != "" && attribute.XMLName.Space != "http://torznab.com/schemas/2015/feed" && attribute.XMLName.Space != "http://www.newznab.com/DTD/2010/feeds/attributes/" {
					continue
				}
				name, value := attribute.Name, attribute.Value
				if name != "seeders" && name != "peers" && name != "downloadvolumefactor" && name != "uploadvolumefactor" && name != "imdbid" {
					continue
				}
				if seen[name] {
					return nil, ErrResponse
				}
				seen[name] = true
				switch name {
				case "seeders", "peers":
					n, err := nonnegativeInteger(value)
					if err != nil {
						return nil, err
					}
					if name == "seeders" {
						seeders = n
					} else {
						peers = n
					}
				case "downloadvolumefactor", "uploadvolumefactor":
					n, err := strconv.ParseFloat(value, 64)
					if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
						return nil, ErrResponse
					}
					if name == "downloadvolumefactor" {
						download = n
						free = n == 0
					} else {
						upload = n
					}
				case "imdbid":
					item.IMDbID = value
				}
			}
			items = append(items, item)
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
		}
	}
	return items, nil
}
