package transmission

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
)

type GlobalRateSession struct {
	client *Client
	token  string
	modern bool
}

func globalRatePayload(modern bool, write bool, args map[string]any) []byte {
	method := "session-get"
	if write {
		method = "session-set"
	}
	var raw []byte
	if modern {
		method = strings.ReplaceAll(method, "-", "_")
		raw, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": args, "id": 1})
	} else {
		raw, _ = json.Marshal(map[string]any{"method": method, "arguments": args})
	}
	return raw
}
func parseGlobalRateReply(raw []byte, modern bool) (map[string]json.RawMessage, error) {
	if modern {
		var r struct {
			JSONRPC string                     `json:"jsonrpc"`
			ID      *int                       `json:"id"`
			Result  map[string]json.RawMessage `json:"result"`
			Error   json.RawMessage            `json:"error"`
		}
		if json.Unmarshal(raw, &r) != nil || r.JSONRPC != "2.0" || r.ID == nil || *r.ID != 1 || hasJSONError(r.Error) || r.Result == nil {
			return nil, ErrResponse
		}
		return r.Result, nil
	}
	var r struct {
		Result    string                     `json:"result"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Result != "success" {
		return nil, ErrResponse
	}
	return r.Arguments, nil
}
func (client *Client) OpenGlobalRates(ctx context.Context) (*GlobalRateSession, speedlimit.Rates, error) {
	s := &GlobalRateSession{client: client}
	raw, status, header, err := client.call(ctx, globalRatePayload(false, false, map[string]any{}), "")
	if err != nil {
		return nil, speedlimit.Rates{}, err
	}
	if status == http.StatusConflict {
		s.token = strings.TrimSpace(header.Get("X-Transmission-Session-Id"))
		s.modern = modernRPC(header)
		if s.token == "" || len(s.token) > 4096 {
			return nil, speedlimit.Rates{}, ErrResponse
		}
		raw, status, _, err = client.call(ctx, globalRatePayload(s.modern, false, map[string]any{}), s.token)
	}
	if err != nil || status != 200 {
		return nil, speedlimit.Rates{}, ErrResponse
	}
	rates, err := s.parse(raw)
	return s, rates, err
}
func (s *GlobalRateSession) parse(raw []byte) (speedlimit.Rates, error) {
	r := speedlimit.Rates{Unit: "RPC KB/s"}
	values, err := parseGlobalRateReply(raw, s.modern)
	if err != nil {
		return r, err
	}
	key := func(v string) string {
		if s.modern {
			return strings.ReplaceAll(v, "-", "_")
		}
		return v
	}
	decode := func(name string, out any) bool {
		value, ok := values[key(name)]
		return ok && string(value) != "null" && json.Unmarshal(value, out) == nil
	}
	if !decode("speed-limit-up", &r.Upload) || !decode("speed-limit-down", &r.Download) || !decode("speed-limit-up-enabled", &r.UploadLimited) || !decode("speed-limit-down-enabled", &r.DownloadLimited) || !decode("alt-speed-enabled", &r.Alternative) || !decode("alt-speed-time-enabled", &r.Scheduled) || !decode("alt-speed-up", &r.OtherUpload) || !decode("alt-speed-down", &r.OtherDownload) || !r.Valid("transmission") {
		return r, ErrResponse
	}
	return r, nil
}
func (s *GlobalRateSession) Read(ctx context.Context) (speedlimit.Rates, error) {
	raw, status, _, err := s.client.call(ctx, globalRatePayload(s.modern, false, map[string]any{}), s.token)
	if err != nil || status != 200 {
		return speedlimit.Rates{}, ErrResponse
	}
	return s.parse(raw)
}

// The read-only handshake has already selected the protocol and CSRF token.
// A conflict/error during this single session-set is uncertain, never replayed.
func (s *GlobalRateSession) Write(ctx context.Context, dimension string, after speedlimit.Rates) error {
	if dimension != "pair" || !after.Valid("transmission") || after.Alternative {
		return ErrConfiguration
	}
	args := map[string]any{}
	for key, value := range map[string]any{"speed-limit-up": after.Upload, "speed-limit-down": after.Download, "speed-limit-up-enabled": after.UploadLimited, "speed-limit-down-enabled": after.DownloadLimited} {
		if s.modern {
			key = strings.ReplaceAll(key, "-", "_")
		}
		args[key] = value
	}
	raw, status, _, err := s.client.call(ctx, globalRatePayload(s.modern, true, args), s.token)
	if err != nil || status != 200 {
		return ErrResponse
	}
	_, err = parseGlobalRateReply(raw, s.modern)
	return err
}
