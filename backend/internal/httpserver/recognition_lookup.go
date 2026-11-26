package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

type recognitionLookup struct {
	allowedKind                            string // Bound by the native resource scope, never supplied by a CLI request.
	Title, Raw, Mode, Kind, Year, Language string
	Season                                 int
	Strict, Localize                       bool
	Append                                 *string
}
type recognitionMatch struct {
	fullDetail bool
	Row        map[string]any `json:"row"`
	TV         bool           `json:"tv"`
	Season     []int          `json:"season,omitempty"`
	Episode    []int          `json:"episode,omitempty"`
}

func recognitionLookupCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var q recognitionLookup
	if readMediaLookup(in, &q) != nil || strings.TrimSpace(q.Title) == "" || len(q.Title) > 4096 || len(q.Raw) > 16384 ||
		(q.Mode != "file" && q.Mode != "title") || (q.Kind != "movie" && q.Kind != "tv" && q.Kind != "anime" && q.Kind != "unknown") {
		return errors.New("invalid recognition lookup")
	}
	settings, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	result, err := lookupParsedMedia(ctx, settings, nil, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func recognitionAPIClient(settings map[string]any, base http.RoundTripper) (*http.Client, func(), error) {
	client := &http.Client{Transport: base}
	proxies := objectValue(objectValue(settings["app"])["proxies"])
	if text(proxies["https"]) == "" {
		return client, func() {}, nil
	}
	var err error
	client.Transport, err = siteProxyTransport(base, proxies, "https")
	return client, client.CloseIdleConnections, err
}

func lookupParsedMedia(ctx context.Context, settings map[string]any, direct http.RoundTripper, q recognitionLookup) (recognitionMatch, error) {
	var result recognitionMatch
	if strings.TrimSpace(text(objectValue(settings["app"])["rmt_tmdbkey"])) == "" {
		return result, errors.New("TMDB API key is unavailable")
	}
	api, closeClient, err := recognitionAPIClient(settings, direct)
	if err != nil {
		return result, err
	}
	defer closeClient()
	kind := "movie"
	if q.Kind == "tv" || q.Mode == "file" && q.Kind != "movie" {
		kind = "tv"
	}
	relax := !q.Strict && !strings.EqualFold(text(objectValue(settings["app"])["rmt_match_mode"]), "strict")
	result.Row, err = matchMediaTitle(ctx, api, settings, titleMatchQuery{Title: q.Title, Kind: kind, FirstYear: q.Year, SeasonYear: q.Year, Season: q.Season, Mode: q.Mode, Relax: relax})
	if result.Row == nil && ctx.Err() == nil {
		next, assistErr := assistRecognition(ctx, settings, direct, api, q)
		if assistErr != nil {
			return next, assistErr
		}
		result = next
	}
	if result.Row == nil {
		return result, err
	}
	if !result.fullDetail && len(slice(result.Row["genres"])) == 0 {
		result.Row, err = fullTMDBDetail(ctx, settings, api.Transport, tmdbDetailQuery{Kind: text(result.Row["media_type"]), ID: text(result.Row["id"]), Append: q.Append, Language: q.Language, Localize: q.Localize})
		return result, err
	}
	return result, nil
}

// Preserve the original lookup order. A later verified match can recover from
// a provider failure, but a partial search must never become a cached negative.
func assistRecognition(ctx context.Context, settings map[string]any, direct http.RoundTripper, api *http.Client, q recognitionLookup) (recognitionMatch, error) {
	var result recognitionMatch
	var incomplete error
	lab := objectValue(settings["laboratory"])
	agent := text(objectValue(settings["app"])["user_agent"])
	if agent == "" {
		agent = defaultSiteUserAgent
	}
	if q.Mode == "title" && truthy(lab["search_tmdbweb"]) && !strings.ContainsFunc(q.Title, func(r rune) bool { return r >= '一' && r <= '鿿' }) {
		doc, err := titleWebPage(ctx, direct, agent, "https://www.themoviedb.org/search", url.Values{"query": {q.Title}})
		if err == nil {
			kind, id := singleWebIdentity(doc, q.Kind == "tv" || q.allowedKind == "tv")
			if id != "" && (q.allowedKind == "" || q.allowedKind == kind) {
				result.Row, err = fullTMDBDetail(ctx, settings, api.Transport, tmdbDetailQuery{Kind: kind, ID: id, Localize: q.Localize, Language: q.Language, Append: q.Append})
			}
		}
		if result.Row != nil {
			result.fullDetail = true
			return result, nil
		}
		incomplete = err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if truthy(lab["chatgpt_enable"]) {
		hint, err := aiTitleHint(ctx, settings, direct, api.Transport, q.Raw)
		if hint != nil {
			result.Season = hintNumbers(hint["season"])
			result.Episode = hintNumbers(hint["episode"])
			result.TV = len(result.Season) > 0 || len(result.Episode) > 0
			name := strings.ReplaceAll(firstHint(hint["title"]), ".", " ")
			year := firstHint(hint["year"])
			if n, e := strconv.Atoi(year); e != nil || len(year) != 4 || n < 1000 {
				year = ""
			}
			if name != "" {
				kind := "tv"
				if q.Kind == "movie" && !result.TV {
					kind = "movie"
					if firstHint(hint["year"]) == "" {
						kind = "multi"
					}
				}
				if kind == "multi" && q.allowedKind != "" {
					kind = q.allowedKind
				}
				if q.allowedKind == "" || kind == q.allowedKind {
					result.Row, err = matchMediaTitle(ctx, api, settings, titleMatchQuery{Title: name, Kind: kind, FirstYear: year})
				}
			}
		}
		if result.Row != nil {
			return result, nil
		}
		if err != nil {
			incomplete = err
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if truthy(lab["search_keyword"]) {
		name, movie, err := guessTitleKeyword(ctx, direct, agent, q.Title)
		if name != "" {
			kind := "multi"
			if movie {
				kind = "movie"
			}
			if kind == "multi" && q.allowedKind != "" {
				kind = q.allowedKind
			}
			if q.allowedKind == "" || kind == q.allowedKind {
				result.Row, err = matchMediaTitle(ctx, api, settings, titleMatchQuery{Title: name, Kind: kind})
			}
		}
		if result.Row != nil {
			return result, nil
		}
		if err != nil {
			incomplete = err
		}
	}
	return result, incomplete
}

func firstHint(v any) string {
	switch v.(type) {
	case string, json.Number:
		head, _, _ := strings.Cut(text(v), "/")
		return strings.TrimSpace(head)
	}
	return ""
}
func hintNumbers(v any) []int {
	items, ok := v.([]any)
	if !ok {
		items = []any{v}
	}
	if len(items) == 0 || len(items) > 1000 {
		return nil
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		var raw string
		switch n := item.(type) {
		case string:
			raw = n
		case json.Number:
			raw = n.String()
		default:
			return nil
		}
		number, err := strconv.Atoi(raw)
		if err != nil || number < 0 || number > 9999 || strings.Trim(raw, "0123456789") != "" {
			return nil
		}
		out = append(out, number)
	}
	return out
}

const recognitionPrompt = "I will give you a movie/tvshow file name.You need to return a Json.\nPay attention to the correct identification of the film name.\n{\"title\":string,\"version\":string,\"part\":string,\"year\":string,\"resolution\":string,\"season\":number|null,\"episode\":number|null}"

func aiTitleHint(ctx context.Context, settings map[string]any, direct, proxied http.RoundTripper, raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	dest, err := sitesignin.QuestionAI(settings)
	if err != nil {
		return nil, err
	}
	if dest.Key == "" {
		return nil, errors.New("recognition service key is unavailable")
	}
	payload, _ := json.Marshal(map[string]any{"model": dest.Model, "user": "NAStool", "messages": []map[string]string{{"role": "system", "content": recognitionPrompt}, {"role": "user", "content": raw}}})
	req, err := http.NewRequestWithContext(ctx, "POST", dest.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid recognition service")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dest.Key)
	transport := direct
	if text(objectValue(settings["openai"])["api_url"]) == "" {
		transport = proxied
	}
	client := http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("recognition service unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("recognition service returned non-success status")
	}
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	var answer struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err != nil || len(rawBody) > 1<<20 || json.Unmarshal(rawBody, &answer) != nil || len(answer.Choices) == 0 {
		return nil, errors.New("invalid recognition service reply")
	}
	var value map[string]any
	d := json.NewDecoder(strings.NewReader(answer.Choices[0].Message.Content))
	d.UseNumber()
	if d.Decode(&value) != nil || value == nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid recognition service title")
	}
	return value, nil
}

// Resource scopes may fix a media type. Recheck it after all auxiliary providers.
// AI episode hints do not constitute evidence of the contents of a torrent.
func resourceTitleAssistance(ctx context.Context, settings map[string]any, base http.RoundTripper, title, raw, kind, forced string) (recognitionMatch, error) {
	lab := objectValue(settings["laboratory"])
	if !truthy(lab["search_tmdbweb"]) && !truthy(lab["chatgpt_enable"]) && !truthy(lab["search_keyword"]) {
		return recognitionMatch{}, nil
	}
	if len(title) > 4096 || len(raw) > 16384 {
		return recognitionMatch{}, errors.New("resource title too long for assistance")
	}
	api, closeClient, err := recognitionAPIClient(settings, base)
	if err != nil {
		return recognitionMatch{}, err
	}
	defer closeClient()
	allowed := forced
	if allowed == "" && kind == "tv" {
		allowed = "tv"
	}
	result, err := assistRecognition(ctx, settings, base, api, recognitionLookup{Title: title, Raw: raw, Kind: kind, Mode: "title", Localize: true, allowedKind: allowed})
	if err != nil || result.Row == nil {
		return recognitionMatch{}, err
	}
	found := text(result.Row["media_type"])
	if forced != "" && forced != found || kind == "tv" && found != "tv" || !numericTMDBID(text(result.Row["id"])) {
		return recognitionMatch{}, nil
	}
	return result, nil
}
