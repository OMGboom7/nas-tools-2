// Package downloadcompletion holds authoritative, bounded file manifests read
// from downloaders. Completion is necessary, never sufficient, for organizing.
package downloadcompletion

import (
	"errors"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrManifest = errors.New("invalid download completion manifest")

func NormalizeIDs(kind string, ids []string) ([]string, error) {
	if len(ids) > 1000 {
		return nil, ErrManifest
	}
	result := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.ToLower(id)
		if !ValidID(kind, id) || seen[id] {
			return nil, ErrManifest
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func ValidID(kind, value string) bool {
	switch kind {
	case "qbittorrent":
		if len(value) != 40 && len(value) != 64 {
			return false
		}
	case "transmission":
		if len(value) != 40 {
			return false
		}
	case "aria2":
		if len(value) != 16 {
			return false
		}
	default:
		return false
	}
	for _, c := range strings.ToLower(value) {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

type File struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Selected bool   `json:"selected"`
	Complete bool   `json:"complete"`
}
type Task struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Directory   string   `json:"directory"`
	State       string   `json:"state"`
	Complete    bool     `json:"complete"`
	LabelsKnown bool     `json:"labelsKnown"`
	Labels      []string `json:"labels"`
	Files       []File   `json:"files"`
}

func validText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func Absolute(s string) bool {
	return validText(s, 4096) && strings.HasPrefix(s, "/") && s != "/" && !strings.Contains(s, "\\") && path.Clean(s) == s
}
func Relative(s string) bool {
	return validText(s, 4096) && s != "" && s != "." && !strings.HasPrefix(s, "/") && !strings.Contains(s, "\\") && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func Normalize(task Task) (Task, error) {
	task.Directory = strings.TrimRight(task.Directory, "/")
	if task.ID == "" || !validText(task.ID, 128) || !validText(task.Name, 4096) || task.Name == "" || !Absolute(task.Directory) || !validText(task.State, 64) || task.State == "" || len(task.Files) == 0 || len(task.Files) > 1000 || len(task.Labels) > 65 {
		return Task{}, ErrManifest
	}
	task.ID = strings.ToLower(task.ID)
	task.Files = append([]File{}, task.Files...)
	seen := map[string]bool{}
	selected := 0
	for _, f := range task.Files {
		if !Relative(f.Path) || f.Size < 0 || seen[f.Path] {
			return Task{}, ErrManifest
		}
		seen[f.Path] = true
		if f.Selected {
			selected++
			task.Complete = task.Complete && f.Complete
		}
	}
	if selected == 0 {
		task.Complete = false
	}
	sort.Slice(task.Files, func(i, j int) bool { return task.Files[i].Path < task.Files[j].Path })
	labels := map[string]bool{}
	task.Labels = append([]string{}, task.Labels...)
	for _, label := range task.Labels {
		if !validText(label, 256) || label == "" || strings.TrimSpace(label) != label || labels[label] {
			return Task{}, ErrManifest
		}
		labels[label] = true
	}
	sort.Strings(task.Labels)
	if len(task.Labels) == 65 && !labels["已整理"] {
		return Task{}, ErrManifest
	}
	return task, nil
}
func (task Task) HasLabel(label string) bool {
	if !task.LabelsKnown {
		return false
	}
	for _, value := range task.Labels {
		if value == label {
			return true
		}
	}
	return false
}
