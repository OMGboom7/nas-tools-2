// Package torrentremover binds future source deletion facts to exact existing
// download/transfer/cross-seed relationships. It never deletes local files.
package torrentremover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

const Plugin = "TorrentRemover"
const ConfigKey = "plugin.TorrentRemover"

var ErrAttempted = errors.New("original task incarnation already has a removal intent")
var ErrState = errors.New("torrent removal source or execution state changed")

type Target struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Digest string `json:"digest"`
}
type Control struct {
	Exclusive bool     `json:"exclusive"`
	Epoch     string   `json:"epoch"`
	Actor     string   `json:"actor"`
	Scope     string   `json:"scope"`
	Config    string   `json:"config"`
	Targets   []Target `json:"targets"`
}
type Event struct {
	ID        string `json:"id"`
	Removal   string `json:"removal"`
	History   string `json:"history"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	Driver    string `json:"driver"`
}
type History struct {
	ID     string `json:"id"`
	Plugin string `json:"plugin"`
	Key    string `json:"key"`
	Value  string `json:"value"`
	Date   string `json:"date"`
}
type Candidate struct {
	ID         string `json:"id"`
	Downloader string `json:"downloader"`
	Task       string `json:"task"`
	Root       string `json:"root"`
	Role       string `json:"role"`
	Parent     string `json:"parent,omitempty"`
}
type Download struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Downloader string `json:"downloader"`
	Task       string `json:"task"`
	SavePath   string `json:"savePath"`
	Date       string `json:"date"`
}
type Definition struct {
	Event          Event       `json:"event"`
	Control        Control     `json:"control"`
	Candidates     []Candidate `json:"candidates"`
	Histories      []History   `json:"histories"`
	Problems       []string    `json:"problems"`
	Downloads      []Download  `json:"downloads"`
	TransferConfig *string     `json:"transferConfig,omitempty"`
}
type Task struct {
	Candidate
	State  string `json:"state"`
	Reason string `json:"reason"`
	Proof  string `json:"-"`
}
type Receipt struct {
	ID         string     `json:"id"`
	EventID    string     `json:"eventId"`
	State      string     `json:"state"`
	Reason     string     `json:"reason"`
	Actor      string     `json:"actor"`
	Created    string     `json:"created"`
	Updated    string     `json:"updated"`
	Definition Definition `json:"-"`
	Tasks      []Task     `json:"tasks"`
}
type Settings struct {
	Raw          string
	InstalledRaw string
	Installed    bool
	Control      Control
}

func Hash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Hex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
func ValidID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}
func validText(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func Enabled(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return false, ErrState
	}
	var enabled bool
	if b, ok := fields["enable"]; ok {
		if string(b) == "null" || json.Unmarshal(b, &enabled) != nil {
			return false, ErrState
		}
	}
	return enabled, nil
}
func Merge(raw string, enabled bool) (string, error) {
	fields := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &fields) != nil || fields == nil) {
		return "", ErrState
	}
	fields["enable"], _ = json.Marshal(enabled)
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	_, err = Enabled(string(b))
	return string(b), err
}
func (c Control) Valid() bool {
	if !c.Exclusive || !Hex(c.Epoch, 64) || !Hex(c.Scope, 64) || !Hex(c.Config, 64) || !validText(c.Actor, 256) || len(c.Targets) == 0 || len(c.Targets) > 50 {
		return false
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if !ValidID(t.ID) || !validText(t.Name, 512) || !Hex(t.Digest, 64) || seen[t.ID] || (t.Type != "qbittorrent" && t.Type != "transmission" && t.Type != "aria2") {
			return false
		}
		seen[t.ID] = true
	}
	return true
}
func (e Event) Valid() bool {
	return Hex(e.ID, 64) && Hex(e.Removal, 64) && ValidID(e.History) && validText(e.Title, 600) && downloadcompletion.Absolute(e.Path) && (e.Driver == "" || e.Driver == "rclone" || e.Driver == "minio")
}
func EventFromPayload(id, reference string, raw json.RawMessage) (Event, error) {
	var wire struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Removal  string `json:"removal_id"`
		History  string `json:"history_id"`
		Driver   string `json:"driver"`
		Media    struct {
			Title string `json:"title"`
		} `json:"media_info"`
	}
	if len(raw) > 128<<10 || json.Unmarshal(raw, &wire) != nil || reference != wire.Removal || (!downloadcompletion.Absolute(wire.Path) && wire.Path != "/") {
		return Event{}, ErrState
	}
	e := Event{ID: id, Removal: reference, History: wire.History, Title: wire.Media.Title, Path: wire.Path, Driver: wire.Driver, Directory: wire.Filename == ""}
	if wire.Filename != "" {
		if !downloadcompletion.Relative(wire.Filename) || path.Base(wire.Filename) != wire.Filename {
			return Event{}, ErrState
		}
		e.Path = path.Join(wire.Path, wire.Filename)
	}
	if !e.Valid() {
		return Event{}, ErrState
	}
	return e, nil
}
func targetFor(c Control, id string) (Target, bool) {
	for _, t := range c.Targets {
		if t.ID == id {
			return t, true
		}
	}
	return Target{}, false
}

// FileMatch retains the old component-suffix relationship, without allowing
// ../, absolute downloader names, or a partial component such as my-movie.mkv
// to match movie.mkv. The caller must independently establish history ownership.
func FileMatch(e Event, relative string) bool {
	if !e.Valid() || !downloadcompletion.Relative(relative) {
		return false
	}
	if !e.Directory {
		return strings.HasSuffix(e.Path, "/"+relative)
	}
	parts := strings.Split(relative, "/")
	for i := 1; i < len(parts); i++ {
		if strings.HasSuffix(e.Path, "/"+strings.Join(parts[:i], "/")) {
			return true
		}
	}
	return false
}
func CandidateID(root, downloader, task, role string) string {
	return Hash([]string{"torrent-removal-candidate", root, downloader, strings.ToLower(task), role})
}
func (d Definition) Valid() bool {
	if !d.Event.Valid() || !d.Control.Valid() || len(d.Candidates) > 2000 || len(d.Histories) > 1000 || len(d.Problems) > 1000 || len(d.Downloads) > 1000 {
		return false
	}
	seen := map[string]bool{}
	for _, c := range d.Candidates {
		t, ok := targetFor(d.Control, c.Downloader)
		if !ok || !downloadcompletion.ValidID(t.Type, c.Task) || c.Task != strings.ToLower(c.Task) || !Hex(c.Root, 64) || c.ID != CandidateID(c.Root, c.Downloader, c.Task, c.Role) || seen[c.ID] || !slices.Contains([]string{"original", "transfer", "cross-seed"}, c.Role) {
			return false
		}
		if c.Role == "cross-seed" && !Hex(c.Parent, 64) {
			return false
		}
		seen[c.ID] = true
	}
	for _, h := range d.Histories {
		if !ValidID(h.ID) || (h.Plugin != "TorrentTransfer" && h.Plugin != "IYUUAutoSeed") || !validText(h.Key, 256) || len(h.Value) > 1<<20 || !json.Valid([]byte(h.Value)) {
			return false
		}
	}
	for _, record := range d.Downloads {
		if !ValidID(record.ID) || !ValidID(record.Downloader) || record.Title != d.Event.Title || !validText(record.Task, 128) || len(record.SavePath) > 4096 {
			return false
		}
	}
	return true
}
