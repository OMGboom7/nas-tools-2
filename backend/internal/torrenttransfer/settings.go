// Package torrenttransfer holds the scoped building blocks of the native
// TorrentTransfer migration. Runtime scheduling is wired separately.
package torrenttransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Plugin = "TorrentTransfer"
const ConfigKey = "plugin.TorrentTransfer"

var ErrState = errors.New("torrent transfer source or settings require review")
var ErrUnsupported = errors.New("torrent transfer file identity unsupported on this platform")

type Settings struct {
	Enable       bool     `json:"enable"`
	OnlyOnce     bool     `json:"onlyonce"`
	Cron         string   `json:"cron"`
	Notify       bool     `json:"notify"`
	NoLabels     string   `json:"nolabels"`
	FromPath     string   `json:"frompath"`
	ToPath       string   `json:"topath"`
	From         []string `json:"fromdownloader"`
	To           []string `json:"todownloader"`
	DeleteSource bool     `json:"deletesource"`
	TorrentPath  string   `json:"fromtorrentpath"`
	NoPaths      string   `json:"nopaths"`
	AutoStart    bool     `json:"autostart"`
}

func Hash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Hex(raw string, n int) bool {
	if len(raw) != n {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
func Parse(raw string) (Settings, error) {
	s := Settings{From: []string{}, To: []string{}}
	if raw == "" {
		return s, nil
	}
	var fields map[string]json.RawMessage
	if len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return s, ErrState
	}
	for key, dst := range map[string]*bool{"enable": &s.Enable, "onlyonce": &s.OnlyOnce, "notify": &s.Notify, "deletesource": &s.DeleteSource, "autostart": &s.AutoStart} {
		if v, ok := fields[key]; ok && json.Unmarshal(v, dst) != nil {
			return s, ErrState
		}
	}
	for key, dst := range map[string]*string{"cron": &s.Cron, "nolabels": &s.NoLabels, "frompath": &s.FromPath, "topath": &s.ToPath, "fromtorrentpath": &s.TorrentPath, "nopaths": &s.NoPaths} {
		if v, ok := fields[key]; ok && json.Unmarshal(v, dst) != nil {
			return s, ErrState
		}
		if len(*dst) > 32<<10 || !utf8.ValidString(*dst) || strings.ContainsRune(*dst, 0) {
			return s, ErrState
		}
	}
	for key, dst := range map[string]*[]string{"fromdownloader": &s.From, "todownloader": &s.To} {
		if v, exists := fields[key]; exists {
			var ids []json.RawMessage
			if json.Unmarshal(v, &ids) != nil || len(ids) > 1 {
				return s, ErrState
			}
			for _, rawID := range ids {
				var id string
				if json.Unmarshal(rawID, &id) != nil {
					id = string(rawID)
				}
				n, err := strconv.ParseInt(id, 10, 64)
				if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
					return s, ErrState
				}
				*dst = append(*dst, id)
			}
		}
	}
	if len(s.FromPath) > 4096 || len(s.ToPath) > 4096 || len(s.TorrentPath) > 4096 || len(strings.Fields(s.Cron)) > 5 || strings.ContainsAny(s.Cron, "\r\n") {
		return s, ErrState
	}
	if _, err := s.ExcludedPaths(); err != nil {
		return s, err
	}
	if _, err := s.ExcludedLabels(); err != nil {
		return s, err
	}
	return s, nil
}
func Merge(raw string, changes map[string]json.RawMessage) (string, error) {
	fields := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &fields) != nil || fields == nil) {
		return "", ErrState
	}
	allowed := []string{"enable", "onlyonce", "cron", "notify", "nolabels", "frompath", "topath", "fromdownloader", "todownloader", "deletesource", "fromtorrentpath", "nopaths", "autostart"}
	for key, value := range changes {
		if !slices.Contains(allowed, key) {
			return "", ErrState
		}
		fields[key] = value
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	_, err = Parse(string(b))
	return string(b), err
}
func (s Settings) Ready() error {
	if !s.Enable || len(s.From) != 1 || len(s.To) != 1 || s.From[0] == s.To[0] || s.TorrentPath == "" || !filepath.IsAbs(s.TorrentPath) || filepath.Clean(s.TorrentPath) == string(filepath.Separator) || strings.ContainsFunc(s.TorrentPath, unicode.IsControl) {
		return ErrState
	}
	if _, err := NormalizeRemote(s.FromPath); s.FromPath != "" && err != nil {
		return err
	}
	if _, err := NormalizeRemote(s.ToPath); s.ToPath != "" && err != nil {
		return err
	}
	return nil
}

// Exclusions retain the original plugin's effective matching rules. In
// particular, upgrading must not admit tasks previously excluded by qB's
// substring tags or the old directory-prefix filter.
func (s Settings) ExcludedPaths() ([]string, error) {
	out := []string{}
	for _, line := range strings.Split(s.NoPaths, "\n") {
		if line == "" {
			continue
		}
		if len(line) > 4096 || !utf8.ValidString(line) || strings.ContainsRune(line, 0) {
			return nil, ErrState
		}
		normalized := legacyDirectory(line)
		if !slices.Contains(out, normalized) {
			if len(out) >= 128 {
				return nil, ErrState
			}
			out = append(out, normalized)
		}
	}
	return out, nil
}
func legacyDirectory(value string) string {
	cleaned := path.Clean(value)
	// POSIX normpath preserves precisely two leading slashes.
	if strings.HasPrefix(value, "//") && !strings.HasPrefix(value, "///") {
		cleaned = "//" + strings.TrimLeft(cleaned, "/")
	}
	return cleaned
}
func (s Settings) ExcludedLabels() ([]string, error) {
	out := []string{}
	for _, label := range strings.Split(s.NoLabels, ",") {
		if len(label) > 256 || strings.ContainsFunc(label, unicode.IsControl) {
			return nil, ErrState
		}
		if !slices.Contains(out, label) {
			if len(out) >= 128 {
				return nil, ErrState
			}
			out = append(out, label)
		}
	}
	return out, nil
}
func (s Settings) Excluded(kind, directory, rawQbTags string, labels []string) (bool, error) {
	if kind != "qbittorrent" && kind != "transmission" {
		return false, ErrState
	}
	if directory != "" {
		if _, err := NormalizeRemote(directory); err != nil {
			return false, err
		}
		prefixes, err := s.ExcludedPaths()
		if err != nil {
			return false, err
		}
		current := legacyDirectory(directory)
		for _, prefix := range prefixes {
			if strings.HasPrefix(current, prefix) {
				return true, nil
			}
		}
	}
	denied, err := s.ExcludedLabels()
	if err != nil {
		return false, err
	}
	if s.NoLabels == "" {
		return false, nil
	}
	if kind == "qbittorrent" && rawQbTags != "" {
		for _, label := range denied {
			if strings.Contains(rawQbTags, label) {
				return true, nil
			}
		}
	}
	if kind == "transmission" && len(labels) > 0 {
		for _, label := range denied {
			if slices.Contains(labels, label) {
				return true, nil
			}
		}
	}
	return false, nil
}

// NormalizeRemote handles downloader-visible POSIX, drive and UNC paths. It
// never treats a textual prefix or a traversal component as a directory scope.
func NormalizeRemote(raw string) (string, error) {
	if raw == "" || len(raw) > 4096 || !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsControl) {
		return "", ErrState
	}
	value := strings.ReplaceAll(raw, "\\", "/")
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", ErrState
		}
	}
	if strings.HasPrefix(value, "//") {
		body := path.Clean(strings.TrimPrefix(value, "//"))
		parts := strings.Split(body, "/")
		if len(parts) < 2 || parts[0] == "" || parts[0] == "." || parts[1] == "" || parts[1] == "." || strings.Contains(body, ":") {
			return "", ErrState
		}
		return "//" + body, nil
	}
	if len(value) >= 3 && value[1] == ':' && value[2] == '/' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		letter := strings.ToUpper(value[:1])
		if strings.Contains(value[2:], ":") {
			return "", ErrState
		}
		return letter + ":" + path.Clean(value[2:]), nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", ErrState
	}
	return path.Clean(value), nil
}
func underRemote(candidate, root string) bool {
	return candidate == root || root == "/" && strings.HasPrefix(candidate, "/") && !strings.HasPrefix(candidate, "//") || strings.HasPrefix(candidate, strings.TrimRight(root, "/")+"/")
}
func MapDirectory(directory, from, to string) (string, error) {
	if directory == "" {
		return NormalizeRemote(to)
	}
	current, err := NormalizeRemote(directory)
	if err != nil {
		return "", err
	}
	if from == "" || to == "" {
		return current, nil
	}
	source, err := NormalizeRemote(from)
	if err != nil {
		return "", err
	}
	target, err := NormalizeRemote(to)
	if err != nil {
		return "", err
	}
	if !underRemote(current, source) {
		return "", ErrState
	}
	relative := strings.TrimPrefix(current, source)
	if source == "/" {
		relative = "/" + strings.TrimPrefix(current, "/")
	}
	if relative == "" || relative == "/" {
		return target, nil
	}
	return NormalizeRemote(strings.TrimRight(target, "/") + "/" + strings.TrimLeft(relative, "/"))
}
