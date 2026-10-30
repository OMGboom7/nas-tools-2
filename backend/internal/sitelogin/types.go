// Package sitelogin records one explicitly requested original-site login.
// Passwords, one-time codes, HTML and captcha images are not journal documents.
package sitelogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

var ErrState = errors.New("site login receipt or original source is unavailable")
var ErrBusy = errors.New("an original site login is already active")
var ErrScope = errors.New("original site login scope changed")
var ErrInput = errors.New("original site login input is not available")

const SavedKey = "CookieUserInfo"
const MaxSites = 50
const MaxOperations = 256

type Source struct {
	SiteID                                       int64
	Site, Application, Catalog, Authority, Saved string
	LoginURL, UserAgent                          string
	UseSaved, OCR                                bool
}
type Operation struct {
	Kind, Signature, State, Digest string
}
type Record struct {
	ID, Batch, Actor, Name, Host, State, Created, Updated string
	Reason, LoginState                                    string
	Need, Nonce, Deadline                                 string
	Source                                                Source
	Operations                                            []Operation
}
type Batch struct {
	ID, Actor, Created, Updated   string
	Jobs                          []string
	Remember                      bool
	RememberState, RememberReason string
}
type Guard func(context.Context) error

func (b Batch) Valid() bool {
	if !Hex(b.ID, 64) || !Text(b.Actor, 128) || b.Actor == "" || len(b.Jobs) == 0 || len(b.Jobs) > MaxSites || !Text(b.RememberReason, 800) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, b.Created); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, b.Updated); err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, id := range b.Jobs {
		if !Hex(id, 64) || seen[id] {
			return false
		}
		seen[id] = true
	}
	if !b.Remember {
		return b.RememberState == "not_requested"
	}
	switch b.RememberState {
	case "pending", "saved", "preserved", "blocked", "uncertain":
		return true
	}
	return false
}

func Hash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func Hex(raw string, size int) bool {
	_, err := hex.DecodeString(raw)
	return err == nil && len(raw) == size
}
func Random() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(raw[:])
}
func Text(v string, max int) bool {
	return utf8.ValidString(v) && len(v) <= max && !strings.ContainsAny(v, "\x00\r\n")
}
func Active(state string) bool {
	return state == "queued" || state == "running" || state == "waiting_input"
}
func (s Source) Valid() bool {
	u, err := url.Parse(s.LoginURL)
	return s.SiteID > 0 && Hex(s.Site, 64) && Hex(s.Application, 64) && Hex(s.Catalog, 64) && Hex(s.Authority, 64) && (!s.UseSaved || Hex(s.Saved, 64)) && err == nil && u.Hostname() != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http") && Text(s.LoginURL, 16000) && Text(s.UserAgent, 512) && s.UserAgent != ""
}
func (r Record) Valid() bool {
	if !Hex(r.ID, 64) || !Hex(r.Batch, 64) || !Text(r.Actor, 128) || r.Actor == "" || !r.Source.Valid() || !Text(r.Name, 512) || !Text(r.Host, 512) || !Text(r.Reason, 800) || len(r.Operations) > MaxOperations {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Created); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Updated); err != nil {
		return false
	}
	switch r.State {
	case "queued", "running", "waiting_input", "completed", "failed", "blocked", "uncertain", "stopped":
	default:
		return false
	}
	if r.Need != "" {
		if r.State != "waiting_input" || !Hex(r.Nonce, 64) {
			return false
		}
		switch r.Need {
		case "captcha", "two_step", "email", "email_code":
		default:
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, r.Deadline); err != nil {
			return false
		}
	}
	for _, op := range r.Operations {
		if !Text(op.Kind, 80) || op.Kind == "" || !Hex(op.Signature, 64) || op.Digest != "" && !Hex(op.Digest, 64) {
			return false
		}
		switch op.State {
		case "intent", "received", "uncertain", "denied":
		default:
			return false
		}
	}
	return true
}
func Interrupted(r Record) string {
	for _, op := range r.Operations {
		if strings.HasPrefix(op.Kind, "action:") || op.State == "intent" || op.State == "uncertain" {
			return "uncertain"
		}
	}
	return "stopped"
}
func SourceMatches(s Source, site siteconfig.Site) bool {
	return s.SiteID == site.ID && s.Site == Hash(site)
}
