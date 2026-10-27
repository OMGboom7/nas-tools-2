// Package torrentfiles describes complete torrent manifests, not completed
// downloads. Indexes belong to a particular downloader response, never a title.
package torrentfiles

import (
	"errors"
	"math"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

var ErrManifest = errors.New("torrent file manifest or selection is invalid")
var ErrNoCoverage = errors.Join(ErrManifest, errors.New("no selected episode is present"))

const MaxFiles = 10000

type File struct {
	Index    int    `json:"index"`
	Path     string `json:"path"`
	Size     int64  `json:"size,string"`
	Selected bool   `json:"selected"`
	Padding  bool   `json:"padding,omitempty"`
}
type Snapshot struct {
	Hash      string   `json:"hash"`
	Directory string   `json:"directory"`
	Labels    []string `json:"labels"`
	State     string   `json:"state"`
	Paused    bool     `json:"paused"`
	Files     []File   `json:"files"`
}

func Relative(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl) && !strings.ContainsAny(value, "\\:") && !strings.HasPrefix(value, "/") && value != "." && value != ".." && !strings.HasPrefix(value, "../") && path.Clean(value) == value
}
func Normalize(files []File) ([]File, error) {
	if len(files) == 0 || len(files) > MaxFiles {
		return nil, ErrManifest
	}
	out := append([]File{}, files...)
	ids := map[int]bool{}
	names := map[string]bool{}
	var size int64
	budget := 0
	for _, f := range out {
		folded := strings.ToLower(f.Path)
		budget += len(f.Path)
		if f.Index < 0 || f.Index > 1000000 || !Relative(f.Path) || f.Size < 0 || f.Size > math.MaxInt64-size || ids[f.Index] || names[folded] || budget > 4<<20 || f.Padding && f.Selected {
			return nil, ErrManifest
		}
		size += f.Size
		ids[f.Index] = true
		names[folded] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	// A file cannot also be another file's parent.
	for name := range names {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if names[parent] {
				return nil, ErrManifest
			}
		}
	}
	return out, nil
}

// Match compares all real files by exact path/length, not list position.
// BEP padding may be hidden by the client; any visible padding stays unwanted.
func Match(expected, actual []File) bool {
	e, err := Normalize(expected)
	if err != nil {
		return false
	}
	a, err := Normalize(actual)
	if err != nil {
		return false
	}
	byPath := map[string]File{}
	for _, f := range e {
		byPath[f.Path] = f
	}
	for _, f := range a {
		old, ok := byPath[f.Path]
		if !ok || old.Size != f.Size || old.Padding && f.Selected {
			return false
		}
		delete(byPath, f.Path)
	}
	for _, f := range byPath {
		if !f.Padding {
			return false
		}
	}
	return true
}
func Wanted(expected, actual []File) bool {
	if !Match(expected, actual) {
		return false
	}
	selected := map[string]bool{}
	for _, f := range expected {
		selected[f.Path] = f.Selected
	}
	for _, f := range actual {
		if f.Selected != selected[f.Path] {
			return false
		}
	}
	return true
}
func IDs(files []File, selected bool) []int {
	out := []int{}
	for _, f := range files {
		if f.Selected == selected {
			out = append(out, f.Index)
		}
	}
	sort.Ints(out)
	return out
}

var chain = regexp.MustCompile(`(?i)(?:s[0-9]{1,2})?(e[0-9]{1,4}(?:e[0-9]{1,4})+)`)
var number = regexp.MustCompile(`(?i)e([0-9]{1,4})`)

func fileEpisodes(name string, total int) ([]int, error) {
	info, err := mediameta.Episodes(name, "")
	if err != nil {
		return nil, err
	}
	if info.Episode == nil {
		return nil, nil
	}
	out := []int{}
	if c := chain.FindString(name); c != "" {
		for _, m := range number.FindAllStringSubmatch(c, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, ErrManifest
			}
			out = append(out, n)
		}
	} else {
		last := *info.Episode
		if info.EndEpisode != nil {
			last = *info.EndEpisode
		}
		if last-*info.Episode > 10000 {
			return nil, ErrManifest
		}
		for n := *info.Episode; n <= last; n++ {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	for i, n := range out {
		if n < 1 || n > total {
			return nil, nil
		}
		if i > 0 && out[i-1] == n {
			return nil, ErrManifest
		}
	}
	return out, nil
}
func video(name string) bool {
	return strings.Contains("|mkv|mp4|avi|mov|wmv|ts|m2ts|iso|rmvb|flv|webm|m4v|mpg|mpeg|", "|"+strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")+"|")
}

// Plan selects only indivisible files wholly within the requested episodes.
// The caller has already verified the torrent's media identity and fixed season.
func Plan(files []File, season, total int, target []int) ([]File, []int, error) {
	return plan(files, season, total, target, false)
}

// PlanExactSeason never assigns an unqualified E01 to several seasons of a
// mixed pack. A filename or its nearest season directory must identify it.
func PlanExactSeason(files []File, season, total int, target []int) ([]File, []int, error) {
	return plan(files, season, total, target, true)
}
func plan(files []File, season, total int, target []int, requireSeason bool) ([]File, []int, error) {
	out, err := Normalize(files)
	if err != nil || season < 0 || season > 1000 || total < 1 || total > 10000 || len(target) == 0 || len(target) > total {
		return nil, nil, ErrManifest
	}
	needed := map[int]bool{}
	for _, n := range target {
		if n < 1 || n > total || needed[n] {
			return nil, nil, ErrManifest
		}
		needed[n] = true
	}
	covered := map[int]bool{}
	stems := map[string]bool{}
	for i := range out {
		f := &out[i]
		f.Selected = false
		if f.Padding || f.Size == 0 || !video(f.Path) {
			continue
		}
		base := path.Base(f.Path)
		low := strings.ToLower(base)
		if strings.HasPrefix(low, "sample.") || strings.Contains(low, ".sample.") || strings.HasPrefix(low, "trailer.") || strings.Contains(low, ".trailer.") {
			continue
		}
		info, err := mediameta.Episodes(base, "")
		if err != nil {
			return nil, nil, ErrManifest
		}
		if info.Season != nil && (*info.Season != season || info.EndSeason != nil && *info.EndSeason != season) {
			continue
		}
		knownSeason := info.Season != nil
		if info.Season == nil {
			// Prefer the nearest explicit directory season; do not inherit a mixed pack.
			for dir := path.Dir(f.Path); dir != "." && dir != "/"; dir = path.Dir(dir) {
				parent, e := mediameta.Episodes(path.Base(dir), "")
				if e != nil {
					return nil, nil, ErrManifest
				}
				if parent.Season != nil {
					knownSeason = true
					if parent.EndSeason != nil && *parent.EndSeason != season {
						info.EndSeason = parent.EndSeason
					}
					if *parent.Season != season || parent.EndSeason != nil && *parent.EndSeason != season {
						info.Season = parent.Season
					}
					break
				}
			}
			if info.Season != nil && *info.Season != season || info.EndSeason != nil && *info.EndSeason != season {
				continue
			}
		}
		if requireSeason && !knownSeason {
			continue
		}
		eps, err := fileEpisodes(base, total)
		if err != nil {
			return nil, nil, err
		}
		if len(eps) == 0 {
			continue
		}
		all := true
		for _, n := range eps {
			all = all && needed[n]
		}
		if !all {
			continue
		}
		f.Selected = true
		for _, n := range eps {
			covered[n] = true
		}
		stems[strings.TrimSuffix(f.Path, path.Ext(f.Path))] = true
	}
	if len(covered) == 0 {
		return nil, nil, ErrNoCoverage
	}
	// Only known same-video companions, never a similarly numbered executable.
	for i := range out {
		f := &out[i]
		if f.Selected || f.Padding {
			continue
		}
		ext := strings.ToLower(path.Ext(f.Path))
		if !slices.Contains([]string{".srt", ".ass", ".ssa", ".sub", ".idx", ".smi", ".vtt", ".nfo"}, ext) {
			continue
		}
		stem := strings.TrimSuffix(f.Path, path.Ext(f.Path))
		for media := range stems {
			if stem == media || ext != ".nfo" && strings.HasPrefix(stem, media+".") {
				f.Selected = true
				break
			}
		}
	}
	episodes := []int{}
	for n := range covered {
		episodes = append(episodes, n)
	}
	sort.Ints(episodes)
	return out, episodes, nil
}
