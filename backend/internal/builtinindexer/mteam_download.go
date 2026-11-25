package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type mteamHTTPStatus int

func (s mteamHTTPStatus) Error() string { return http.StatusText(int(s)) }
func (s mteamHTTPStatus) Unwrap() error { return ErrResponse }

type mteamLocator struct {
	address, api *url.URL
	id           string // Empty for an already signed RSS enclosure.
}

func parseMTeamLocator(raw string) (mteamLocator, bool) {
	var target mteamLocator
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Fragment != "" || !externalindexer.ValidDownloadURL(raw) {
		return target, false
	}
	signed := u.Path == "/api/rss/dl" || strings.HasPrefix(u.Path, "/api/rss/dl/")
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	_, api, err := mteamBase(origin)
	if signed && strings.EqualFold(u.Host, "api.m-team.info") {
		api = &url.URL{Scheme: "https", Host: u.Host}
	} else if err != nil {
		return target, false
	}
	if strings.Contains(u.Path, "\\") || strings.ContainsFunc(u.Path, unicode.IsControl) {
		return target, false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return target, false
		}
	}
	target = mteamLocator{address: u, api: api}
	if signed {
		return target, true
	}
	id, ok := strings.CutPrefix(u.EscapedPath(), "/detail/")
	n, e := strconv.ParseInt(id, 10, 64)
	if !ok || e != nil || n < 1 || strconv.FormatInt(n, 10) != id {
		return mteamLocator{}, false
	}
	target.id = id
	return target, true
}

func IsMTeamDownload(raw string) bool {
	_, ok := parseMTeamLocator(raw)
	return ok
}

// A saved front end may also supply signed RSS links on its own fixed API host.
// Detail-page token issuance always requires the exact saved origin.
func MTeamDownloadMatches(source, raw string) bool {
	target, ok := parseMTeamLocator(raw)
	base, err := url.Parse(source)
	if !ok || err != nil || base.User != nil {
		return false
	}
	if sameOrigin(base, target.address) {
		return true
	}
	_, api, err := mteamBase((&url.URL{Scheme: base.Scheme, Host: base.Host}).String())
	return target.id == "" && err == nil && sameOrigin(api, target.address)
}

// A signed enclosure carries its own authorization, so it may use a site's
// separately configured RSS origin without granting that origin its API key.
func IsSignedMTeamDownload(raw string) bool {
	target, ok := parseMTeamLocator(raw)
	return ok && target.id == ""
}

type mteamFile struct {
	content  []byte
	filename string
	magnet   string
}

func fetchMTeamFile(ctx context.Context, raw, key, ua string, transport http.RoundTripper) (mteamFile, error) {
	var empty mteamFile
	target, ok := parseMTeamLocator(raw)
	if !ok || strings.ContainsAny(ua, "\r\n") {
		return empty, ErrConfig
	}
	link := raw
	if target.id != "" {
		form := url.Values{"id": {target.id}}
		body, err := mteamRequest(ctx, target.api, "/api/torrent/genDlToken", key, ua, "application/x-www-form-urlencoded", []byte(form.Encode()), transport)
		if err != nil {
			return empty, err
		}
		if json.Unmarshal(body, &link) != nil || !externalindexer.ValidDownloadURL(link) {
			return empty, ErrResponse
		}
		signed, err := url.Parse(link)
		if err != nil || signed.Fragment != "" || !sameOrigin(target.api, signed) {
			return empty, ErrResponse
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return empty, ErrConfig
	}
	req.Header.Set("User-Agent", ua)
	client := http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) > 3 || next.URL.User != nil || next.URL.Fragment != "" || !sameOrigin(req.URL, next.URL) {
			return http.ErrUseLastResponse
		}
		// net/http normally copies the preceding signed URL into Referer.
		next.Header.Del("Referer")
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, ErrResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return empty, mteamHTTPStatus(resp.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(content) > 8<<20 {
		return empty, ErrResponse
	}
	if len(content) <= 4096 && bytes.HasPrefix(content, []byte("magnet:?")) && externalindexer.ValidDownloadURL(string(content)) {
		return mteamFile{magnet: string(content)}, nil
	}
	meta, err := torrentmeta.Parse(content)
	if err != nil {
		return empty, ErrResponse
	}
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	name := params["filename"]
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	if !safeTorrentBasename(name) {
		name = meta.Name
	}
	if !safeTorrentBasename(name) {
		name = meta.InfoHash
	}
	if !strings.HasSuffix(strings.ToLower(name), ".torrent") {
		name += ".torrent"
	}
	return mteamFile{content: content, filename: name}, nil
}

func safeTorrentBasename(name string) bool {
	return len(name) > 0 && len(name) <= 200 && utf8.ValidString(name) && filepath.IsLocal(name) && name != "." && name != ".." && strings.TrimRight(name, ". ") == name && !strings.ContainsAny(name, "/\\:*?\"<>|") && !strings.ContainsFunc(name, unicode.IsControl)
}

// DownloadMTeam returns validated metainfo for a cached search result.
func DownloadMTeam(ctx context.Context, raw, key, ua string, transport http.RoundTripper) ([]byte, error) {
	f, err := fetchMTeamFile(ctx, raw, key, ua, transport)
	if err == nil && f.magnet != "" {
		return nil, ErrResponse
	}
	return f.content, err
}

// FetchMTeamLink also accepts the magnet body supported by manual and RSS clients.
func FetchMTeamLink(ctx context.Context, raw, key, ua string, transport http.RoundTripper) ([]byte, error) {
	f, err := fetchMTeamFile(ctx, raw, key, ua, transport)
	if f.magnet != "" {
		return []byte(f.magnet), nil
	}
	return f.content, err
}

// The legacy downloader requires a recognizable basename and a local file.
// Separate concurrent requests must never overwrite another caller's metainfo.
func saveMTeamFile(dir string, f mteamFile) (string, error) {
	name := filepath.Join(dir, f.filename)
	out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		out, err = os.CreateTemp(dir, strings.TrimSuffix(f.filename, filepath.Ext(f.filename))+"-*.torrent")
	}
	if err != nil {
		return "", err
	}
	name = out.Name()
	_, writeErr := out.Write(f.content)
	closeErr := out.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func MTeamDownloadCommand(ctx context.Context, in io.Reader, out io.Writer) error {
	var q struct {
		URL, Key, UA, Directory string
		Proxy                   *string
	}
	input := &io.LimitedReader{R: in, N: 128 << 10}
	d := json.NewDecoder(input)
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || !filepath.IsAbs(q.Directory) {
		return ErrConfig
	}
	var transport http.RoundTripper
	if q.Proxy != nil {
		var err error
		transport, err = siteconfig.ProxyTransport(nil, *q.Proxy)
		if err != nil {
			return ErrConfig
		}
	}
	f, err := fetchMTeamFile(ctx, q.URL, q.Key, q.UA, transport)
	var status mteamHTTPStatus
	if errors.As(err, &status) {
		return json.NewEncoder(out).Encode(map[string]int{"status": int(status)})
	}
	if err != nil {
		return err
	}
	if f.magnet != "" {
		return json.NewEncoder(out).Encode(map[string]string{"magnet": f.magnet})
	}
	name, err := saveMTeamFile(q.Directory, f)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]string{"path": name})
}
