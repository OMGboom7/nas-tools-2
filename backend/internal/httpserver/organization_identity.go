package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func decodeOrganizationRequest(w http.ResponseWriter, r *http.Request, target any, message string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(w, 400, 400, message)
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAPIError(w, 400, 400, message)
		return false
	}
	return true
}

// Selection is bound to a specific file snapshot from the readonly preview,
// never applied recursively to a folder or guessed onto another file.
type organizationMediaAssignment struct {
	Source     string `json:"source"`
	Identity   string `json:"identity"`
	Modified   string `json:"modified"`
	Size       int64  `json:"size"`
	Kind       string `json:"kind"`
	TMDBID     string `json:"tmdbId"`
	Season     *int   `json:"season,omitempty"`
	Episode    *int   `json:"episode,omitempty"`
	EndEpisode *int   `json:"endEpisode,omitempty"`
}

func organizationAssignments(input []organizationMediaAssignment, files []organization.File) (map[string]organizationMediaAssignment, *recognitionFailure) {
	result := map[string]organizationMediaAssignment{}
	if len(input) > 100 {
		return nil, &recognitionFailure{422, "manual media selection is limited to 100 files"}
	}
	for _, selection := range input {
		id, err := strconv.ParseInt(selection.TMDBID, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != selection.TMDBID || selection.Kind != "movie" && selection.Kind != "tv" || selection.Source == "" || selection.Identity == "" || selection.Modified == "" || selection.Size < 0 {
			return nil, &recognitionFailure{422, "invalid manual media selector or file snapshot"}
		}
		if _, exists := result[selection.Source]; exists {
			return nil, &recognitionFailure{422, "duplicate manual media selection"}
		}
		if selection.Kind == "movie" {
			if selection.Season != nil || selection.Episode != nil || selection.EndEpisode != nil {
				return nil, &recognitionFailure{422, "movie identity must not include season or episode selection"}
			}
		} else {
			if selection.Season == nil || selection.Episode == nil || *selection.Season < 0 || *selection.Season > 99 || *selection.Episode < 1 || *selection.Episode > 9999 {
				return nil, &recognitionFailure{422, "TV identity requires an explicit valid season and episode"}
			}
			if selection.EndEpisode != nil && (*selection.EndEpisode < *selection.Episode || *selection.EndEpisode > 9999) {
				return nil, &recognitionFailure{422, "invalid TV episode range"}
			}
		}
		found := false
		for _, file := range files {
			if file.Path != selection.Source {
				continue
			}
			if file.Kind != "media" {
				return nil, &recognitionFailure{422, "manual identity can only select a scanned media file"}
			}
			if file.Identity != selection.Identity || file.Size != selection.Size || strconv.FormatInt(file.Modified, 10) != selection.Modified {
				return nil, &recognitionFailure{409, "selected source changed; review a fresh preview before assigning identity"}
			}
			found = true
			break
		}
		if !found {
			return nil, &recognitionFailure{422, "manual selection is outside the scanned media set"}
		}
		result[selection.Source] = selection
	}
	return result, nil
}

func (api mediaNameAPI) recognizeAssigned(ctx context.Context, title string, selection organizationMediaAssignment) (*nativeRecognition, *recognitionFailure) {
	episodes := mediameta.EpisodeInfo{}
	if selection.Kind == "tv" {
		episodes = mediameta.EpisodeInfo{TV: true, Season: selection.Season, Episode: selection.Episode, EndEpisode: selection.EndEpisode, Count: 1}
		if selection.EndEpisode != nil {
			episodes.Count = *selection.EndEpisode - *selection.Episode + 1
		}
	}
	return api.recognizeSelected(ctx, title, "", selection.Kind, selection.TMDBID, &episodes)
}
