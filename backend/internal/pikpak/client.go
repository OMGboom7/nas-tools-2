// Package pikpak implements the account and offline-download operations used by
// NAS Tools. It deliberately does not replay ambiguous submissions.
package pikpak

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid PikPak configuration")
	ErrConnection     = errors.New("PikPak connection failed")
	ErrAuthentication = errors.New("PikPak authentication failed or account verification required")
	ErrResponse       = errors.New("invalid PikPak response")
	ErrNotSubmitted   = errors.New("PikPak submission was not attempted")
)

// Public application identifiers, not NAS Tools account credentials. Protocol
// reference: Quan666/PikPakAPI, pikpakapi/__init__.py and utils.py.
const clientID = "YNxT9w7GMdWvEOKa"
const clientSecret = "dbw2OtmVEeuUvIptb1Coyg"
const accountURL = "https://user.mypikpak.com"
const driveURL = "https://api-drive.mypikpak.com"

type Client struct {
	username, password, device string
	http                       *http.Client
	mu                         sync.Mutex
	token                      string
}

func New(username, password, proxy string, transport http.RoundTripper) (*Client, error) {
	if !safeText(username, 1024) || !safeText(password, 4096) || proxy != "" && !safeText(proxy, 4096) {
		return nil, ErrConfiguration
	}
	username, proxy = strings.TrimSpace(username), strings.TrimSpace(proxy)
	if !safeText(username, 1024) || !safeText(password, 4096) {
		return nil, ErrConfiguration
	}
	if proxy != "" {
		if !strings.Contains(proxy, "://") {
			proxy = "http://" + proxy
		}
		u, err := url.Parse(proxy)
		if err != nil || u.Hostname() == "" || u.Opaque != "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || strings.ContainsAny(proxy, "\r\n") {
			return nil, ErrConfiguration
		}
		base := http.DefaultTransport
		if transport != nil {
			base = transport
		}
		t, ok := base.(*http.Transport)
		if !ok {
			return nil, ErrConfiguration
		}
		clone := t.Clone()
		clone.Proxy = http.ProxyURL(u)
		transport = clone
	}
	// Match the upstream library's stable device identity; never expose or log it.
	digest := md5.Sum([]byte(username + password))
	return &Client{username: username, password: password, device: hex.EncodeToString(digest[:]), http: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func safeText(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func ValidID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (c *Client) request(ctx context.Context, method, address string, body any, token string, target any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return ErrConfiguration
		}
		reader = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return ErrConfiguration
	}
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-Device-Id", c.device)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return ErrConnection
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ErrAuthentication
	}
	if resp.StatusCode != 200 {
		return ErrResponse
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return ErrResponse
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil || envelope == nil {
		return ErrResponse
	}
	if raw, ok := envelope["error"]; ok && string(raw) != "null" && string(raw) != `""` {
		return ErrAuthentication
	}
	if raw, ok := envelope["error_code"]; ok && string(raw) != "0" && string(raw) != "null" {
		return ErrResponse
	}
	if json.Unmarshal(data, target) != nil {
		return ErrResponse
	}
	return nil
}

// login is called under mu. An interactive challenge is not bypassed or
// redirected to another service, and no upstream diagnostics leave this package.
func (c *Client) login(ctx context.Context) error {
	meta := map[string]string{"username": c.username}
	if strings.Contains(c.username, "@") {
		meta = map[string]string{"email": c.username}
	} else {
		phone := len(c.username) >= 11 && len(c.username) <= 18
		for _, r := range c.username {
			phone = phone && r >= '0' && r <= '9'
		}
		if phone {
			meta = map[string]string{"phone_number": c.username}
		}
	}
	var challenge struct {
		Token string `json:"captcha_token"`
		URL   string `json:"url"`
	}
	if err := c.request(ctx, http.MethodPost, accountURL+"/v1/shield/captcha/init", map[string]any{"client_id": clientID, "action": "POST:" + accountURL + "/v1/auth/signin", "device_id": c.device, "meta": meta}, "", &challenge); err != nil {
		return err
	}
	if !safeText(challenge.Token, 16<<10) || challenge.URL != "" {
		return ErrAuthentication
	}
	var session struct {
		Token   string `json:"access_token"`
		Subject string `json:"sub"`
		Type    string `json:"token_type"`
	}
	if err := c.request(ctx, http.MethodPost, accountURL+"/v1/auth/signin", map[string]any{"client_id": clientID, "client_secret": clientSecret, "username": c.username, "password": c.password, "captcha_token": challenge.Token}, "", &session); err != nil {
		return err
	}
	if !safeText(session.Token, 16<<10) || !ValidID(session.Subject) || session.Type != "" && !strings.EqualFold(session.Type, "bearer") {
		return ErrAuthentication
	}
	c.token = session.Token
	return nil
}

func (c *Client) Check(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	return c.login(ctx)
}

type Task struct{ ID, Name, Phase string }

func (c *Client) Tasks(ctx context.Context) ([]Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}
	result := []Task{}
	pages, ids := map[string]bool{}, map[string]bool{}
	page := ""
	for n := 0; n < 100; n++ {
		q := url.Values{"type": {"offline"}, "thumbnail_size": {"SIZE_SMALL"}, "limit": {"100"}, "page_token": {page}, "filters": {`{"phase":{"in":"PHASE_TYPE_RUNNING,PHASE_TYPE_ERROR,PHASE_TYPE_PENDING"}}`}, "with": {"reference_resource"}}
		var payload struct {
			Tasks *[]struct {
				ID    string `json:"id"`
				Name  string `json:"file_name"`
				Phase string `json:"phase"`
			} `json:"tasks"`
			Next string `json:"next_page_token"`
		}
		if err := c.request(ctx, http.MethodGet, driveURL+"/drive/v1/tasks?"+q.Encode(), nil, c.token, &payload); err != nil {
			return nil, err
		}
		if payload.Tasks == nil || len(*payload.Tasks) > 100 {
			return nil, ErrResponse
		}
		for _, t := range *payload.Tasks {
			if !ValidID(t.ID) || !safeText(strings.TrimSpace(t.Name), 4096) || ids[t.ID] {
				return nil, ErrResponse
			}
			ids[t.ID] = true
			switch t.Phase {
			case "PHASE_TYPE_RUNNING", "PHASE_TYPE_ERROR", "PHASE_TYPE_PENDING":
				result = append(result, Task{t.ID, strings.TrimSpace(t.Name), t.Phase})
			case "PHASE_TYPE_COMPLETE":
			default:
				return nil, ErrResponse
			}
		}
		if payload.Next == "" {
			return result, nil
		}
		if !safeText(payload.Next, 4096) || pages[payload.Next] {
			return nil, ErrResponse
		}
		pages[payload.Next] = true
		page = payload.Next
	}
	return nil, ErrResponse
}

// AddMagnet returns the case-sensitive cloud task ID, not the torrent hash.
// No retry, token refresh or captcha retry can replay this POST.
func (c *Client) AddMagnet(ctx context.Context, magnet, parentID string) (string, error) {
	if len(magnet) > 4096 || !strings.HasPrefix(magnet, "magnet:?") || !safeText(magnet, 4096) || parentID != "" && !ValidID(parentID) {
		return "", ErrConfiguration
	}
	u, err := url.Parse(magnet)
	if err != nil || u.Host != "" || u.Fragment != "" {
		return "", ErrConfiguration
	}
	for _, r := range magnet {
		if r <= 0x20 {
			return "", ErrConfiguration
		}
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["xt"]) != 1 || !strings.HasPrefix(strings.ToLower(q.Get("xt")), "urn:btih:") {
		return "", ErrConfiguration
	}
	hash := q.Get("xt")[9:]
	if len(hash) == 40 {
		if _, err := hex.DecodeString(hash); err != nil {
			return "", ErrConfiguration
		}
	} else if len(hash) == 32 {
		if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(hash)); err != nil {
			return "", ErrConfiguration
		}
	} else {
		return "", ErrConfiguration
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			return "", errors.Join(ErrNotSubmitted, err)
		}
	}
	folder := "DOWNLOAD"
	if parentID != "" {
		folder = ""
	}
	var payload struct {
		Task struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	err = c.request(ctx, http.MethodPost, driveURL+"/drive/v1/files", map[string]any{"kind": "drive#file", "name": nil, "upload_type": "UPLOAD_TYPE_URL", "url": map[string]string{"url": magnet}, "folder_type": folder, "parent_id": parentID}, c.token, &payload)
	if err != nil {
		return "", err
	}
	if !ValidID(payload.Task.ID) {
		return "", ErrResponse
	}
	return payload.Task.ID, nil
}
