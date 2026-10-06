package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

type mediaNameAPI struct {
	service    subscriptionService
	categories mediaCategoryAPI
}

func (api mediaNameAPI) serveAPIKey(response http.ResponseWriter, request *http.Request) {
	if api.service.configStore == nil {
		writeAPIError(response, 503, 503, "native media recognition is unavailable")
		return
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		writeAPIError(response, 500, 500, "security configuration is unavailable")
		return
	}
	expected := text(objectValue(snapshot["security"])["api_key"])
	match := func(value string) bool {
		if expected == "" || value == "" || len(value) > 4096 {
			return false
		}
		wanted, supplied := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(value))
		return subtle.ConstantTimeCompare(wanted[:], supplied[:]) == 1
	}
	header := request.Header.Get("Authorization")
	if len(header) <= 4096 {
		fields := strings.Fields(header)
		if len(fields) != 0 && match(fields[len(fields)-1]) {
			api.serveHTTP(response, request)
			return
		}
	}
	if match(request.URL.Query().Get("apikey")) {
		api.serveHTTP(response, request)
		return
	}
	writeAPIError(response, 401, 401, "安全认证未通过，请检查ApiKey")
}

func (api mediaNameAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if api.service.words == nil || api.service.configStore == nil || api.service.systemConfig == nil {
		writeAPIError(response, 503, 503, "native media recognition is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "invalid media recognition request")
		return
	}
	title, subtitle := request.Form.Get("name"), request.Form.Get("subtitle")
	if strings.TrimSpace(title) == "" {
		writeJSON(response, 200, map[string]any{"code": -1})
		return
	}
	if len(title) > 64<<10 || len(subtitle) > 64<<10 {
		writeAPIError(response, 400, 400, "media recognition input too large")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	result, failure := api.recognize(ctx, title, subtitle)
	if failure != nil {
		writeAPIError(response, failure.status, failure.status, failure.message)
		return
	}
	if result == nil {
		writeUnrecognizedMedia(response)
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "data": result.data})
}

type nativeRecognition struct {
	data   map[string]any
	meta   mediameta.Metadata
	detail tmdbMediaDetails
	kind   string
}

type recognitionFailure struct {
	status  int
	message string
}

func (api mediaNameAPI) recognize(ctx context.Context, title, subtitle string) (*nativeRecognition, *recognitionFailure) {
	return api.recognizeKind(ctx, title, subtitle, "")
}

func (api mediaNameAPI) recognizeKind(ctx context.Context, title, subtitle, forcedKind string) (*nativeRecognition, *recognitionFailure) {
	if forcedKind != "" && forcedKind != "movie" && forcedKind != "tv" {
		return nil, &recognitionFailure{422, "invalid media type"}
	}
	if api.service.words == nil || api.service.configStore == nil || api.service.systemConfig == nil {
		return nil, &recognitionFailure{503, "native media recognition is unavailable"}
	}
	_, words, err := api.service.words.List(ctx)
	if err != nil {
		return nil, &recognitionFailure{500, "custom words are unavailable"}
	}
	processed, err := wordconfig.ProcessWords(ctx, title, words)
	if err != nil {
		return nil, &recognitionFailure{400, "invalid media title"}
	}
	processedSubtitle, err := wordconfig.ProcessWords(ctx, subtitle, words)
	if err != nil {
		return nil, &recognitionFailure{400, "invalid media subtitle"}
	}
	options, err := mediameta.ReadLabelOptions(ctx, api.service.systemConfig)
	if err != nil {
		return nil, &recognitionFailure{500, "media label configuration is unavailable"}
	}
	meta, err := mediameta.ParseWithOptions(ctx, processed.Title, processedSubtitle.Title, options)
	if err != nil {
		return nil, &recognitionFailure{400, "invalid media metadata"}
	}
	if meta.Title == "" {
		return nil, nil
	}
	kind, mediaType := "movie", "MOV"
	if forcedKind == "tv" || forcedKind == "" && meta.Episodes.TV {
		kind, mediaType = "tv", "TV"
	}
	meta.Episodes.TV = kind == "tv"
	input := subscriptionUpsertRequest{Name: meta.Title, Year: meta.Year, Type: mediaType}
	id, err := api.service.resolveNativeMediaID(ctx, input, true)
	if err == nil && id == "" && !meta.Episodes.TV && forcedKind == "" {
		input.Type, kind = "TV", "tv"
		id, err = api.service.resolveNativeMediaID(ctx, input, true)
	}
	if err != nil {
		return nil, &recognitionFailure{502, "media identity lookup failed"}
	}
	if id == "" {
		return nil, nil
	}
	meta.Episodes.TV = kind == "tv"
	detail, err := fetchNativeTMDBDetails(ctx, api.service.configStore, api.service.client.Transport, kind, id)
	if err != nil {
		return nil, &recognitionFailure{502, "media details are unavailable"}
	}
	verifiedTitle, date, typeName := detail.Title, detail.ReleaseDate, "电影"
	if kind == "tv" {
		verifiedTitle, date, typeName = detail.Name, detail.FirstAirDate, "电视剧"
	}
	if strings.TrimSpace(verifiedTitle) == "" {
		return nil, &recognitionFailure{502, "invalid media details"}
	}
	categoryKind := kind
	config, err := api.service.configStore.Snapshot()
	if err != nil {
		return nil, &recognitionFailure{500, "media configuration is unavailable"}
	}
	if kind == "tv" && text(objectValue(config["media"])["anime_path"]) != "" {
		for _, genre := range categoryArray(detail.Attributes["genres"]) {
			if object, ok := genre.(map[string]any); ok && number(object["id"]) == 16 {
				categoryKind, typeName = "anime", "动漫"
				break
			}
		}
	}
	category, err := api.categories.match(categoryKind, detail.Attributes)
	if err != nil {
		return nil, &recognitionFailure{502, "media category configuration is unavailable"}
	}
	year := meta.Year
	if len(date) >= 4 {
		if _, err := strconv.Atoi(date[:4]); err == nil {
			year = date[:4]
		}
	}
	link := "https://www.themoviedb.org/" + kind + "/" + id
	seasonEpisode, episodeLink := "", ""
	if meta.Episodes.Season != nil {
		seasonEpisode = fmt.Sprintf("S%02d", *meta.Episodes.Season)
		if meta.Episodes.EndSeason != nil {
			seasonEpisode += fmt.Sprintf("-S%02d", *meta.Episodes.EndSeason)
		}
		episodeLink = link + "/season/" + strconv.Itoa(*meta.Episodes.Season)
	}
	if meta.Episodes.Episode != nil {
		seasonEpisode += fmt.Sprintf("E%02d", *meta.Episodes.Episode)
		if meta.Episodes.EndEpisode != nil {
			seasonEpisode += fmt.Sprintf("-E%02d", *meta.Episodes.EndEpisode)
		}
		if episodeLink != "" {
			episodeLink += "/episode/" + strconv.Itoa(*meta.Episodes.Episode)
		}
	}
	return &nativeRecognition{meta: meta, detail: detail, kind: kind, data: map[string]any{
		"type": typeName, "name": meta.Title, "title": verifiedTitle, "year": year,
		"season_episode": seasonEpisode, "part": "", "tmdbid": detail.ID, "tmdblink": link, "tmdb_S_E_link": episodeLink,
		"category": category, "restype": meta.Source, "effect": meta.Effect, "pix": meta.Resolution,
		"team": meta.Team, "customization": meta.Customization, "video_codec": meta.VideoCodec, "audio_codec": meta.AudioCodec,
		"org_string": title, "rev_string": processed.Title, "ignored_words": processed.Ignored, "replaced_words": processed.Replaced, "offset_words": processed.Offsets,
	}}, nil
}

func writeUnrecognizedMedia(response http.ResponseWriter) {
	writeJSON(response, 200, map[string]any{"code": 0, "data": map[string]any{"name": "无法识别"}})
}
