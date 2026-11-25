// Package iyuu implements the IYUU reseed API used by the bundled plugin.
package iyuu

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const Plugin = "IYUUAutoSeed"
const ConfigKey = "plugin." + Plugin
const origin = "https://2025.iyuu.cn"

var ErrInput = errors.New("invalid IYUU request")
var ErrResponse = errors.New("unexpected IYUU response")
var ErrRejected = errors.New("IYUU rejected the request")

type Client struct {
	Token     string
	Transport http.RoundTripper
}

func ValidToken(value string) bool {
	return value != "" && len(value) <= 2048 && strings.IndexFunc(value, func(r rune) bool { return r < 33 || r > 126 }) < 0
}
func (c Client) call(ctx context.Context, path string, input any, out any) error {
	if !ValidToken(c.Token) {
		return ErrInput
	}
	method := http.MethodGet
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return ErrInput
		}
		method = http.MethodPost
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+path, body)
	if err != nil {
		return ErrInput
	}
	req.Header.Set("token", c.Token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Transport: c.Transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrResponse
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrResponse
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return ErrResponse
	}
	var envelope struct {
		Code *int
		Data json.RawMessage
	}
	if json.Unmarshal(b, &envelope) != nil || envelope.Code == nil {
		return ErrResponse
	}
	if *envelope.Code != 0 {
		return ErrRejected
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) || json.Unmarshal(envelope.Data, out) != nil {
		return ErrResponse
	}
	return nil
}

type Partner struct {
	ID       json.Number `json:"id"`
	Site     string      `json:"site"`
	Nickname string      `json:"nickname"`
}

func positiveID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}
func (c Client) Partners(ctx context.Context) ([]Partner, error) {
	var result struct{ List []Partner }
	if err := c.call(ctx, "/reseed/sites/recommend", nil, &result); err != nil {
		return nil, err
	}
	if result.List == nil || len(result.List) > 4096 {
		return nil, ErrResponse
	}
	seen := map[string]bool{}
	for _, p := range result.List {
		if !positiveID(string(p.ID)) || seen[string(p.ID)] || p.Site == "" || len(p.Site) > 128 || len(p.Nickname) > 256 || strings.ContainsFunc(p.Site+p.Nickname, unicode.IsControl) {
			return nil, ErrResponse
		}
		seen[string(p.ID)] = true
	}
	return result.List, nil
}

func Digest(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
func ValidBinding(uid, passkey string) bool {
	return positiveID(uid) && passkey != "" && len(passkey) <= 1024 && !strings.ContainsFunc(passkey, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}
func (c Client) Bind(ctx context.Context, partner Partner, uid, passkey string) error {
	if !positiveID(string(partner.ID)) || partner.Site == "" || !ValidBinding(uid, passkey) {
		return ErrInput
	}
	var result []json.RawMessage
	err := c.call(ctx, "/reseed/users/bind", map[string]string{"token": c.Token, "site": partner.Site, "sid": string(partner.ID), "id": uid, "passkey": Digest(passkey)}, &result)
	if err != nil {
		return err
	}
	if result == nil || len(result) != 0 {
		return ErrResponse
	}
	return nil
}

type Site struct {
	ID           json.Number `json:"id"`
	Name         string      `json:"site"`
	Domain       string      `json:"base_url"`
	DownloadPage string      `json:"download_page"`
}
type Candidate struct {
	SiteID    json.Number `json:"sid"`
	TorrentID json.Number `json:"torrent_id"`
	Hash      string      `json:"info_hash"`
}

func (c Client) Sites(ctx context.Context) ([]Site, error) {
	var result struct{ Sites []Site }
	if err := c.call(ctx, "/reseed/sites/index", nil, &result); err != nil {
		return nil, err
	}
	if result.Sites == nil || len(result.Sites) > 8192 {
		return nil, ErrResponse
	}
	seen := map[json.Number]bool{}
	for _, s := range result.Sites {
		if !positiveID(string(s.ID)) || seen[s.ID] || s.Domain == "" || len(s.Domain) > 2048 || len(s.DownloadPage) > 8192 {
			return nil, ErrResponse
		}
		seen[s.ID] = true
	}
	return result.Sites, nil
}
func (c Client) ReportSites(ctx context.Context, sites []Site) (string, error) {
	ids := make([]json.Number, 0, len(sites))
	for _, site := range sites {
		ids = append(ids, site.ID)
	}
	var result struct {
		Signature string `json:"sid_sha1"`
	}
	if err := c.call(ctx, "/reseed/sites/reportExisting", map[string]any{"sid_list": ids}, &result); err != nil {
		return "", err
	}
	if !Hash(result.Signature) {
		return "", ErrResponse
	}
	return result.Signature, nil
}

func Hash(s string) bool {
	if len(s) != 40 || strings.ToLower(s) != s {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}

func (c Client) Match(ctx context.Context, hashes []string, signature string, now time.Time) (map[string][]Candidate, error) {
	if len(hashes) == 0 || len(hashes) > 200 || !Hash(signature) {
		return nil, ErrInput
	}
	allowed := map[string]bool{}
	for _, h := range hashes {
		if !Hash(h) || allowed[h] {
			return nil, ErrInput
		}
		allowed[h] = true
	}
	ordered := slices.Clone(hashes)
	slices.Sort(ordered)
	b, _ := json.Marshal(ordered)
	var raw json.RawMessage
	if err := c.call(ctx, "/reseed/index/index", map[string]any{"hash": string(b), "sha1": Digest(string(b)), "sid_sha1": signature, "timestamp": now.Unix(), "version": "8.2.0"}, &raw); err != nil {
		return nil, err
	}
	out := map[string][]Candidate{}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[]")) {
		return out, nil
	}
	var rows map[string]struct{ Torrent json.RawMessage }
	if json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil, ErrResponse
	}
	for source, row := range rows {
		if !allowed[source] {
			return nil, ErrResponse
		}
		if len(row.Torrent) == 0 || string(row.Torrent) == "null" {
			continue
		}
		var list []Candidate
		if row.Torrent[0] == '{' {
			var one Candidate
			if json.Unmarshal(row.Torrent, &one) != nil {
				return nil, ErrResponse
			}
			list = []Candidate{one}
		} else if json.Unmarshal(row.Torrent, &list) != nil {
			return nil, ErrResponse
		}
		for _, v := range list {
			if !positiveID(string(v.SiteID)) || !positiveID(string(v.TorrentID)) || !Hash(v.Hash) {
				return nil, ErrResponse
			}
			if v.Hash != source {
				out[source] = append(out[source], v)
			}
		}
	}
	return out, nil
}
