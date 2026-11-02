package sitesignin

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

const Plugin = "AutoSignIn"

type Settings struct {
	Enabled      bool     `json:"enabled"`
	Cron         string   `json:"cron"`
	Sites        []string `json:"sign_sites"`
	Special      []string `json:"special_sites"`
	RetryKeyword string   `json:"retry_keyword"`
	Concurrency  int      `json:"queue_cnt"`
	Notify       bool     `json:"notify"`
	Once         bool     `json:"onlyonce"`
	Clean        bool     `json:"clean"`
	AutoCF       int      `json:"auto_cf"`
	Missed       bool     `json:"missed_detection"`
	MissedWindow string   `json:"missed_schedule"`
}

func rawSettings(raw string) (map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if raw == "" {
		return values, nil
	}
	if len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return nil, ErrState
	}
	return values, nil
}
func settingText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if d.Decode(&v) == nil {
		if n, ok := v.(json.Number); ok {
			return n.String(), nil
		}
	}
	return "", ErrState
}
func settingIDs(raw json.RawMessage) ([]string, error) {
	out := []string{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || len(list) > MaxSites {
		return nil, ErrState
	}
	seen := map[string]bool{}
	for _, r := range list {
		str, err := settingText(r)
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(str, 10, 64)
		if err != nil || id <= 0 {
			return nil, ErrState
		}
		canonical := strconv.FormatInt(id, 10)
		if seen[canonical] {
			return nil, ErrState
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}
func ParseSettings(raw string) (Settings, error) {
	out := Settings{Concurrency: 10, Sites: []string{}, Special: []string{}}
	v, err := rawSettings(raw)
	if err != nil {
		return out, err
	}
	for key, target := range map[string]*bool{"enabled": &out.Enabled, "notify": &out.Notify, "onlyonce": &out.Once, "clean": &out.Clean, "missed_detection": &out.Missed} {
		if value, ok := v[key]; ok && string(value) != "null" {
			if json.Unmarshal(value, target) != nil {
				return out, ErrState
			}
		}
	}
	for key, target := range map[string]*string{"cron": &out.Cron, "retry_keyword": &out.RetryKeyword, "missed_schedule": &out.MissedWindow} {
		*target, err = settingText(v[key])
		if err != nil || len(*target) > 2000 {
			return out, ErrState
		}
	}
	if out.Sites, err = settingIDs(v["sign_sites"]); err != nil {
		return out, err
	}
	if out.Special, err = settingIDs(v["special_sites"]); err != nil {
		return out, err
	}
	for key, target := range map[string]*int{"queue_cnt": &out.Concurrency, "auto_cf": &out.AutoCF} {
		str, e := settingText(v[key])
		if e != nil {
			return out, e
		}
		if str != "" {
			n, e := strconv.Atoi(str)
			if e != nil || n < 0 || n > 1000 {
				return out, ErrState
			}
			if key == "queue_cnt" && n == 0 {
				n = 10
			}
			*target = n
		}
	}
	if out.RetryKeyword != "" {
		if _, err = regexp2.Compile(out.RetryKeyword, regexp2.None); err != nil {
			return out, ErrState
		}
	}
	if out.Missed {
		if _, _, err = Window(out.MissedWindow); err != nil {
			return out, err
		}
	}
	return out, nil
}

// MergeSettings updates only declared fields. Existing unknown values retain
// their raw JSON representation, including integers larger than 2^53.
func MergeSettings(original string, updates map[string]json.RawMessage) (string, error) {
	v, err := rawSettings(original)
	if err != nil {
		return "", err
	}
	allowed := map[string]bool{}
	for _, key := range strings.Fields("enabled cron sign_sites special_sites retry_keyword queue_cnt notify onlyonce clean auto_cf missed_detection missed_schedule") {
		allowed[key] = true
	}
	for key, value := range updates {
		if !allowed[key] || !json.Valid(value) {
			return "", ErrState
		}
		v[key] = value
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if _, err = ParseSettings(string(raw)); err != nil {
		return "", err
	}
	return string(raw), nil
}

// Window uses local calendar minutes. 24:00 retains the original interpretation
// of 23:59; an invalid or reversed range must never start a hidden timer.
func Window(raw string) (int, int, error) {
	raw = strings.Join(strings.Fields(raw), "")
	raw = strings.ReplaceAll(raw, "24:00", "23:59")
	parts := strings.Split(raw, "-")
	if len(parts) != 2 {
		return 0, 0, ErrState
	}
	parse := func(s string) (int, error) {
		if len(s) != 5 || s[2] != ':' {
			return 0, ErrState
		}
		h, e := strconv.Atoi(s[:2])
		if e != nil {
			return 0, e
		}
		m, e := strconv.Atoi(s[3:])
		if e != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, ErrState
		}
		return h*60 + m, nil
	}
	a, e := parse(parts[0])
	if e != nil {
		return 0, 0, e
	}
	b, e := parse(parts[1])
	if e != nil || a >= b {
		return 0, 0, ErrState
	}
	return a, b, nil
}
func (c Settings) Retry(out Outcome) bool {
	if !out.Retryable || out.State != "failed" && out.State != "deferred" {
		return false
	}
	if c.RetryKeyword == "" || out.State == "deferred" {
		return true
	}
	r, err := regexp2.Compile(c.RetryKeyword, regexp2.None)
	if err != nil {
		return false
	}
	r.MatchTimeout = 50 * time.Millisecond
	yes, err := r.MatchString(out.Reason)
	return err == nil && yes
}

// Select requires an explicit scope; an empty list stays empty. Unknown,
// interrupted and active actions are never automatically repeated, even for a
// special site. A manual review can deliberately choose a new action later.
func (c Settings) Select(sources []Source, daily map[int64]Job, missed bool) []Source {
	special := map[string]bool{}
	for _, id := range c.Special {
		special[id] = true
	}
	out := []Source{}
	for _, source := range sources {
		previous, exists := daily[source.SiteID]
		if !exists {
			out = append(out, source)
			continue
		}
		if Active(previous.State) || previous.Outcome.State == "unknown" || previous.Outcome.State == "stopped" {
			continue
		}
		retryOutcome := previous.Outcome
		retryOutcome.Reason = "【" + source.Name + "】" + retryOutcome.Reason
		if c.Retry(retryOutcome) || !missed && special[strconv.FormatInt(source.SiteID, 10)] && (previous.Outcome.State == "signed" || previous.Outcome.State == "already" || previous.Outcome.State == "authenticated") {
			out = append(out, source)
		}
	}
	return out
}
