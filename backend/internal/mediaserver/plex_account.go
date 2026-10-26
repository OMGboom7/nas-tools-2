package mediaserver

// Compatibility for the existing username/password/servername configuration.
// The legacy password endpoint is deprecated by Plex; use only after a fresh,
// explicit migration consent. No PIN flow, password rotation or auth fallback
// is silently attempted. Account/server tokens remain in process memory.
import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func plexAccountToken(v string) bool {
	return len(v) > 0 && len(v) <= 4096 && !strings.ContainsAny(v, "\x00\r\n\t ")
}
func ResolvePlexAccount(ctx context.Context, username, password, serverName, clientID string, allowHTTP bool, transport http.RoundTripper) (*Client, error) {
	if username == "" || password == "" || serverName == "" || len(username) > 4096 || len(password) > 4096 || len(serverName) > 512 || !refreshHex(clientID, 32) {
		return nil, ErrConfiguration
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	h := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, endpoint, token, body string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
		if err != nil {
			return nil, ErrConfiguration
		}
		req.Header.Set("X-Plex-Client-Identifier", clientID)
		req.Header.Set("X-Plex-Product", "NAS Tools Go")
		req.Header.Set("X-Plex-Version", "1")
		req.Header.Set("Accept", "application/json, application/xml")
		if token != "" {
			req.Header.Set("X-Plex-Token", token)
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := h.Do(req)
		if err != nil {
			return nil, ErrConnection
		}
		defer resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, ErrAuthentication
		}
		if resp.StatusCode != 200 && resp.StatusCode != 201 {
			return nil, ErrResponse
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(b) > 1<<20 {
			return nil, ErrResponse
		}
		return b, nil
	}
	body := url.Values{"user[login]": {username}, "user[password]": {password}}.Encode()
	b, err := send("POST", "https://plex.tv/users/sign_in.json", "", body)
	if err != nil {
		return nil, err
	}
	var signed struct {
		User *struct {
			Token       string `json:"authToken"`
			LegacyToken string `json:"authentication_token"`
		} `json:"user"`
	}
	if json.Unmarshal(b, &signed) != nil || signed.User == nil {
		return nil, ErrResponse
	}
	token := signed.User.Token
	if token == "" {
		token = signed.User.LegacyToken
	} else if signed.User.LegacyToken != "" && token != signed.User.LegacyToken {
		return nil, ErrResponse
	}
	if !plexAccountToken(token) {
		return nil, ErrResponse
	}
	b, err = send("GET", "https://plex.tv/api/resources?includeHttps=1&includeRelay=1&includeIPv6=1", token, "")
	if err != nil {
		return nil, err
	}
	type connection struct {
		URI      string `xml:"uri,attr"`
		Protocol string `xml:"protocol,attr"`
		Local    string `xml:"local,attr"`
		Relay    string `xml:"relay,attr"`
	}
	type resource struct {
		Name        string       `xml:"name,attr"`
		ID          string       `xml:"clientIdentifier,attr"`
		Provides    string       `xml:"provides,attr"`
		Token       string       `xml:"accessToken,attr"`
		Connections []connection `xml:"Connection"`
	}
	var resources struct {
		XMLName xml.Name
		Size    *int       `xml:"size,attr"`
		Devices []resource `xml:"Device"`
	}
	if xml.Unmarshal(b, &resources) != nil || resources.XMLName.Local != "MediaContainer" || resources.Size != nil && *resources.Size != len(resources.Devices) || len(resources.Devices) > 1024 {
		return nil, ErrResponse
	}
	matched := []resource{}
	for _, d := range resources.Devices {
		provides := false
		for _, p := range strings.Split(d.Provides, ",") {
			provides = provides || strings.TrimSpace(p) == "server"
		}
		if provides && strings.EqualFold(d.Name, serverName) {
			matched = append(matched, d)
		}
	}
	if len(matched) != 1 {
		return nil, ErrConfiguration
	}
	d := matched[0]
	if !validInventoryID(d.ID) || !plexAccountToken(d.Token) || len(d.Connections) == 0 || len(d.Connections) > 32 {
		return nil, ErrResponse
	}
	score := func(c connection) int {
		n := 0
		if c.Protocol != "https" {
			n += 10
		}
		if c.Local != "1" && c.Local != "true" {
			n++
		}
		if c.Relay == "1" || c.Relay == "true" {
			n += 2
		}
		return n
	}
	sort.SliceStable(d.Connections, func(i, j int) bool {
		a, b := score(d.Connections[i]), score(d.Connections[j])
		if a != b {
			return a < b
		}
		return d.Connections[i].URI < d.Connections[j].URI
	})
	seen := map[string]bool{}
	for _, c := range d.Connections {
		if ctx.Err() != nil {
			return nil, ErrConnection
		}
		u, err := url.Parse(c.URI)
		if err != nil || u.Scheme != c.Protocol || u.Scheme != "https" && u.Scheme != "http" {
			return nil, ErrResponse
		}
		if u.Scheme == "http" && !allowHTTP {
			continue
		}
		if seen[c.URI] {
			continue
		}
		seen[c.URI] = true
		client, err := New("plex", c.URI, d.Token, transport)
		if err != nil {
			return nil, ErrResponse
		}
		id, err := client.RefreshIdentity(ctx)
		if err == nil && id == d.ID {
			return client, nil
		}
	}
	return nil, ErrConnection
}
