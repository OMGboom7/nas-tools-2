package sitesignin

import (
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type Source struct {
	SiteID                           int64
	Name, URL, UserAgent, SiteDigest string
	Browser                          bool
}
type Scope struct {
	ID, Actor, Authority, ConfigDigest, PluginDigest, SavedDigest string
	Created, Location                                             string
	Sources                                                       []Source
	Options                                                       Options
	Notify                                                        bool
}
type Operation struct {
	Kind, Signature, State, Digest string
	Action                         bool
}
type Job struct {
	ID, Batch, Actor, ScopeID, Day, Created, Updated string
	Source                                           Source
	State                                            string
	Outcome                                          Outcome
	Operations                                       []Operation
	Need, Nonce, Deadline                            string
}
type Batch struct {
	ID, Trigger, Created string
	Scope                Scope
	Jobs                 []string
	Notified             bool
	Summary              string
}

func Active(state string) bool {
	return state == "queued" || state == "running" || state == "waiting_input"
}
func validOutcome(o Outcome) bool {
	if !sitelogin.Text(o.Reason, 800) || len(o.CacheState) > 30 {
		return false
	}
	switch o.State {
	case "signed", "already", "authenticated":
		return o.Authenticated && !o.Retryable
	case "deferred", "failed":
		return true
	case "unknown", "blocked", "stopped":
		return !o.Retryable
	}
	return false
}
func validSource(s Source) bool {
	return s.SiteID > 0 && sitelogin.Hex(s.SiteDigest, 64) && sitelogin.Text(s.Name, 512) && browserruntime.Origin(s.URL) != "" && sitelogin.Text(s.URL, 16000) && sitelogin.Text(s.UserAgent, 512) && s.UserAgent != ""
}
func (s Scope) Valid() bool {
	if !sitelogin.Hex(s.ID, 64) || !sitelogin.Text(s.Actor, 128) || s.Actor == "" || !sitelogin.Hex(s.Authority, 64) || !sitelogin.Hex(s.ConfigDigest, 64) || !sitelogin.Hex(s.PluginDigest, 64) || len(s.Sources) == 0 || len(s.Sources) > MaxSites || s.Options.SavedLogin && !sitelogin.Hex(s.SavedDigest, 64) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, s.Created); err != nil {
		return false
	}
	if _, err := time.LoadLocation(s.Location); err != nil {
		return false
	}
	seen := map[int64]bool{}
	for _, v := range s.Sources {
		if !validSource(v) || seen[v.SiteID] {
			return false
		}
		seen[v.SiteID] = true
	}
	return true
}
func (r Job) Valid() bool {
	if !sitelogin.Hex(r.ID, 64) || !sitelogin.Hex(r.Batch, 64) || !sitelogin.Hex(r.ScopeID, 64) || !sitelogin.Text(r.Actor, 128) || r.Actor == "" || !validSource(r.Source) || len(r.Operations) > 256 {
		return false
	}
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return false
	}
	for _, v := range []string{r.Created, r.Updated} {
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			return false
		}
	}
	if !Active(r.State) && r.State != "finished" {
		return false
	}
	if r.State == "finished" && !validOutcome(r.Outcome) {
		return false
	}
	if r.State == "waiting_input" {
		if !sitelogin.Hex(r.Nonce, 64) {
			return false
		}
		if r.Need != "captcha" && r.Need != "choice" && r.Need != "two_step" && r.Need != "email" && r.Need != "email_code" {
			return false
		}
		if _, err := time.Parse(time.RFC3339Nano, r.Deadline); err != nil {
			return false
		}
	} else if r.Need != "" || r.Nonce != "" || r.Deadline != "" {
		return false
	}
	for _, op := range r.Operations {
		if !sitelogin.Text(op.Kind, 80) || op.Kind == "" || !sitelogin.Hex(op.Signature, 64) || op.Digest != "" && !sitelogin.Hex(op.Digest, 64) {
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
