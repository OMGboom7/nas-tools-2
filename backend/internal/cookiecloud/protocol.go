package cookiecloud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Cookie struct {
	Domain       string          `json:"domain"`
	Name         string          `json:"name"`
	Value        string          `json:"value"`
	Path         string          `json:"path"`
	HostOnly     *bool           `json:"hostOnly"`
	Secure       bool            `json:"secure"`
	Expiration   float64         `json:"expirationDate"`
	PartitionKey json.RawMessage `json:"partitionKey"`
}

func Fetch(ctx context.Context, s Settings, transport http.RoundTripper) ([]Cookie, error) {
	if !s.Ready() || !keyPattern.MatchString(s.Key) {
		return nil, ErrConfig
	}
	server, err := serverURL(s.Server)
	if err != nil {
		return nil, err
	}
	server.Path += "/get/" + s.Key
	body, _ := json.Marshal(map[string]string{"password": s.Password})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrConfig
	}
	r.GetBody = nil
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(r)
	if err != nil {
		return nil, ErrCloud
	}
	defer res.Body.Close()
	const max = 8 << 20
	if res.StatusCode != http.StatusOK || res.ContentLength > max {
		return nil, ErrCloud
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil || len(raw) > max {
		return nil, ErrCloud
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return nil, ErrCloud
	}
	if nested, exists := document["cookie_data"]; exists {
		// Unmarshal into a fresh map to keep only the nested Cookie data.
		document = nil
		if json.Unmarshal(nested, &document) != nil {
			return nil, ErrCloud
		}
	}
	if len(document) > 4096 {
		return nil, ErrCloud
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []Cookie{}
	for _, key := range keys {
		var cookies []Cookie
		if json.Unmarshal(document[key], &cookies) != nil || len(out)+len(cookies) > 50000 {
			return nil, ErrCloud
		}
		for _, cookie := range cookies {
			if strings.TrimSpace(cookie.Domain) == "" {
				continue
			}
			out = append(out, cookie)
		}
	}
	return out, nil
}
