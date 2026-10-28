package mediaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
)

type playingSession struct {
	address string
	bitrate int64
}

func sessionBitrate(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	s := string(raw)
	if len(s) > 2 && s[0] == '"' {
		if json.Unmarshal(raw, &s) != nil {
			return 0, ErrResponse
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > 1<<40 {
		return 0, ErrResponse
	}
	return n, nil
}

// ObservePlaying reads the configured server's complete live session response.
// A failed, truncated or ambiguous response cannot authorize idle limits.
func (client *Client) ObservePlaying(ctx context.Context, spec speedlimit.Spec) (speedlimit.Observation, error) {
	var observation speedlimit.Observation
	sessions, err := client.playingSessions(ctx)
	if err != nil {
		return observation, err
	}
	for _, s := range sessions {
		exempt, err := spec.ExemptAddress(s.address)
		if err != nil {
			return observation, err
		}
		if exempt {
			observation.Exempt++
			continue
		}
		observation.Playing++
		observation.Bitrate += s.bitrate
		if s.bitrate == 0 {
			observation.UnknownBitrate++
		}
	}
	if observation.Bitrate > 1<<50 {
		return observation, ErrResponse
	}
	return observation, nil
}
func (client *Client) playingSessions(ctx context.Context) ([]playingSession, error) {
	if client.kind == "plex" {
		raw, err := client.plexGET(ctx, "/status/sessions", url.Values{})
		if err != nil {
			return nil, err
		}
		return parsePlexSessions(raw)
	}
	endpoint := *client.endpoint
	if client.kind == "emby" {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/emby/System/Info") + "/emby/Sessions"
	} else {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/System/Info") + "/Sessions"
	}
	type sessionRow struct {
		Remote string `json:"RemoteEndPoint"`
		Item   *struct {
			MediaType string          `json:"MediaType"`
			Bitrate   json.RawMessage `json:"Bitrate"`
			Streams   []struct {
				Bitrate json.RawMessage `json:"BitRate"`
			} `json:"MediaStreams"`
		} `json:"NowPlayingItem"`
		State *struct {
			Paused *bool `json:"IsPaused"`
		} `json:"PlayState"`
	}
	var documents []json.RawMessage
	if err := client.readJSON(ctx, endpoint.String(), &documents); err != nil {
		return nil, err
	}
	if documents == nil || len(documents) > 1000 {
		return nil, ErrResponse
	}
	out := []playingSession{}
	for _, document := range documents {
		if _, err := webhookJSON(document); err != nil {
			return nil, ErrResponse
		}
		var row sessionRow
		if json.Unmarshal(document, &row) != nil {
			return nil, ErrResponse
		}
		if row.Item == nil {
			continue
		}
		if row.Item.MediaType == "Audio" || row.Item.MediaType == "Photo" {
			continue
		}
		if row.Item.MediaType != "Video" || row.State == nil || row.State.Paused == nil {
			return nil, ErrResponse
		}
		if *row.State.Paused {
			continue
		}
		var bitrate int64
		if client.kind == "emby" {
			n, err := sessionBitrate(row.Item.Bitrate)
			if err != nil {
				return nil, err
			}
			bitrate = n
		} else {
			if len(row.Item.Streams) > 1000 {
				return nil, ErrResponse
			}
			for _, stream := range row.Item.Streams {
				n, err := sessionBitrate(stream.Bitrate)
				if err != nil {
					return nil, err
				}
				bitrate += n
			}
		}
		if bitrate > 1<<40 {
			return nil, ErrResponse
		}
		out = append(out, playingSession{row.Remote, bitrate})
	}
	return out, nil
}
func parsePlexSessions(raw []byte) ([]playingSession, error) {
	out := []playingSession{}
	add := func(state, address string, bitrates []json.RawMessage) error {
		switch state {
		case "paused", "stopped":
			return nil
		case "playing", "buffering":
		default:
			return ErrResponse
		}
		var bitrate int64
		for _, value := range bitrates {
			n, err := sessionBitrate(value)
			if err != nil || n > 1<<30 {
				return ErrResponse
			}
			bitrate += n * 1000
		}
		if bitrate > 1<<40 {
			return ErrResponse
		}
		out = append(out, playingSession{address, bitrate})
		return nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		if _, err := webhookJSON(raw); err != nil {
			return nil, ErrResponse
		}
		var doc struct {
			Container *struct {
				Size *int `json:"size"`
				Rows []struct {
					Type   string `json:"type"`
					Player *struct {
						State   string `json:"state"`
						Address string `json:"address"`
					} `json:"Player"`
					Media []struct {
						Bitrate json.RawMessage `json:"bitrate"`
					} `json:"Media"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(raw, &doc) != nil || doc.Container == nil || doc.Container.Size == nil || *doc.Container.Size < 0 || *doc.Container.Size > 1000 || *doc.Container.Size != len(doc.Container.Rows) {
			return nil, ErrResponse
		}
		for _, row := range doc.Container.Rows {
			switch row.Type {
			case "track", "photo":
				continue
			case "movie", "episode", "clip", "video":
			default:
				return nil, ErrResponse
			}
			if row.Player == nil || len(row.Media) > 1000 {
				return nil, ErrResponse
			}
			rates := []json.RawMessage{}
			for _, media := range row.Media {
				rates = append(rates, media.Bitrate)
			}
			if err := add(row.Player.State, row.Player.Address, rates); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if !completeSessionXML(raw) {
		return nil, ErrResponse
	}
	var doc struct {
		XMLName xml.Name
		Size    *int `xml:"size,attr"`
		Video   []struct {
			Player []struct {
				State   string `xml:"state,attr"`
				Address string `xml:"address,attr"`
			} `xml:"Player"`
			Media []struct {
				Bitrate string `xml:"bitrate,attr"`
			} `xml:"Media"`
		} `xml:"Video"`
		Track []struct{} `xml:"Track"`
		Photo []struct{} `xml:"Photo"`
	}
	if xml.Unmarshal(raw, &doc) != nil || doc.XMLName.Local != "MediaContainer" || doc.Size == nil || *doc.Size < 0 || *doc.Size > 1000 || *doc.Size != len(doc.Video)+len(doc.Track)+len(doc.Photo) {
		return nil, ErrResponse
	}
	for _, row := range doc.Video {
		if len(row.Player) != 1 || len(row.Media) > 1000 {
			return nil, ErrResponse
		}
		rates := []json.RawMessage{}
		for _, media := range row.Media {
			if media.Bitrate == "" {
				rates = append(rates, nil)
			} else {
				rates = append(rates, json.RawMessage(media.Bitrate))
			}
		}
		if err := add(row.Player[0].State, row.Player[0].Address, rates); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func completeSessionXML(raw []byte) bool {
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, count, roots := 0, 0, 0
	for {
		t, err := d.Token()
		if err == io.EOF {
			return depth == 0 && roots == 1
		}
		if err != nil {
			return false
		}
		count++
		if count > 20000 {
			return false
		}
		switch t := t.(type) {
		case xml.StartElement:
			if depth == 1 && t.Name.Local != "Video" && t.Name.Local != "Track" && t.Name.Local != "Photo" {
				return false
			}
			if depth == 0 {
				roots++
				if roots > 1 {
					return false
				}
			}
			depth++
			if depth > 20 {
				return false
			}
			seen := map[xml.Name]bool{}
			for _, a := range t.Attr {
				if seen[a.Name] {
					return false
				}
				seen[a.Name] = true
			}
		case xml.EndElement:
			depth--
			if depth < 0 {
				return false
			}
		case xml.Directive:
			return false
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return false
			}
		}
	}
}
