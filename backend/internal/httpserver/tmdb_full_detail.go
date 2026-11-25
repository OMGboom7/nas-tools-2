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

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

type tmdbDetailQuery struct {
	Kind, ID, Language string
	Append             *string
	Season             *int
	Localize           bool
}

func TMDBDetailCommand(ctx context.Context, configPath string, in io.Reader, out io.Writer) error {
	var q tmdbDetailQuery
	input := &io.LimitedReader{R: in, N: 16 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 ||
		(q.Kind != "movie" && q.Kind != "tv") || !numericTMDBID(q.ID) ||
		q.Season != nil && (q.Kind != "tv" || *q.Season < 0 || q.Append != nil) {
		return errors.New("invalid TMDB detail query")
	}
	number, _ := strconv.ParseInt(strings.TrimSpace(q.ID), 10, 64)
	q.ID = strconv.FormatInt(number, 10)
	settings, err := config.NewStore(configPath).Snapshot()
	if err != nil {
		return err
	}
	var transport http.RoundTripper
	proxies := objectValue(objectValue(settings["app"])["proxies"])
	if text(proxies["https"]) != "" {
		transport, err = siteProxyTransport(nil, proxies, "https")
		if err != nil {
			return err
		}
		defer (&http.Client{Transport: transport}).CloseIdleConnections()
	}
	detail, err := fullTMDBDetail(ctx, settings, transport, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(detail)
}

// Keep the provider's complete object so plugins can read credits, images and
// fields unknown to the native naming and inventory projections.
func fullTMDBDetail(ctx context.Context, settings map[string]any, transport http.RoundTripper, q tmdbDetailQuery) (map[string]any, error) {
	const defaults = "alternative_titles,translations,external_ids,releases"
	appendFields := defaults
	if q.Append != nil {
		appendFields = *q.Append
		if appendFields == "all" {
			appendFields = "images,credits," + defaults
		}
	}
	media := objectValue(settings["media"])
	extra := map[string]string{"append_to_response": appendFields, "include_adult": strconv.FormatBool(truthy(media["tmdb_include_adult"]))}
	path := "/" + q.Kind + "/" + q.ID
	if q.Season != nil {
		path += "/season/" + strconv.Itoa(*q.Season)
		delete(extra, "append_to_response")
	}
	raw, err := requestTMDBDetail(ctx, settings, transport, path, q.Language, extra, 32<<20)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result == nil {
		return nil, errors.New("invalid TMDB detail response")
	}
	id, ok := result["id"].(json.Number)
	if !ok || !numericTMDBID(id.String()) || result["success"] == false || result["errors"] != nil {
		return nil, errors.New("unverified TMDB detail response")
	}
	if q.Season != nil {
		n, ok := result["season_number"].(json.Number)
		if !ok || n.String() != strconv.Itoa(*q.Season) || result["show_id"] != nil && text(result["show_id"]) != q.ID {
			return nil, errors.New("unexpected TMDB season identity")
		}
		for _, item := range slice(result["episodes"]) {
			episode := objectValue(item)
			if episode["show_id"] != nil && text(episode["show_id"]) != q.ID ||
				episode["season_number"] != nil && text(episode["season_number"]) != n.String() {
				return nil, errors.New("unexpected TMDB episode identity")
			}
		}
		return result, nil
	}
	if id.String() != q.ID || result["media_type"] != nil && result["media_type"] != q.Kind {
		return nil, errors.New("unexpected TMDB title identity")
	}
	genreIDs := []any{}
	for _, genre := range slice(result["genres"]) {
		genreIDs = append(genreIDs, objectValue(genre)["id"])
	}
	result["genre_ids"] = genreIDs
	result["media_type"] = q.Kind
	field, aliases := "title", "titles"
	if q.Kind == "tv" {
		field, aliases = "name", "results"
	}
	if _, ok := result[field]; !ok {
		return nil, errors.New("missing TMDB title field")
	}
	defaultLanguage := text(media["tmdb_language"])
	if q.Localize && (defaultLanguage == "" || defaultLanguage == "zh") && !strings.ContainsFunc(text(result[field]), func(r rune) bool { return r >= '一' && r <= '鿿' }) {
		for _, a := range slice(objectValue(result["alternative_titles"])[aliases]) {
			alias := objectValue(a)
			title := text(alias["title"])
			if text(alias["iso_3166_1"]) == "CN" && scraper.IsSimplifiedChinese(title) {
				result[field] = title
				break
			}
		}
	}
	return result, nil
}
