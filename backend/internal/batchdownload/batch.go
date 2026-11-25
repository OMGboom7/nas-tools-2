// Package batchdownload chooses ranked resources against the current missing
// episodes. The callback returns after a downloader acknowledgment.
package batchdownload

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

var ErrBatch = errors.New("invalid batch download")

type Season struct {
	Number   int   `json:"season"`
	Total    int   `json:"total_episodes"`
	Episodes []int `json:"episodes"`
}
type Candidate struct {
	Show, Title, Display, Label, Resource, Site, Seeders string
	Movie                                                bool
	Seasons, Episodes                                    []int
}
type Input struct {
	Candidates []Candidate
	Missing    map[string][]Season
	Order      string
}
type Proof struct {
	Hash    string
	Targets []Season
	Mixed   bool
	Whole   bool
}
type Action struct {
	Op      string `json:"op"`
	Index   int    `json:"index"`
	Proof   *Proof `json:"proof,omitempty"`
	Partial bool   `json:"partial,omitempty"`
}
type reply struct {
	Accepted bool
	Torrent  []byte
	Pending  bool
}
type Result struct {
	Op       string              `json:"op"`
	Accepted []int               `json:"accepted"`
	Missing  map[string][]Season `json:"missing"`
}

func episodes(s Season) []int {
	if len(s.Episodes) > 0 {
		return s.Episodes
	}
	out := make([]int, s.Total)
	for i := range out {
		out[i] = i + 1
	}
	return out
}
func subset(needed, values []int) bool {
	if len(values) == 0 {
		return false
	}
	available := make(map[int]bool, len(needed))
	for _, v := range needed {
		available[v] = true
	}
	for _, v := range values {
		if !available[v] {
			return false
		}
	}
	return true
}

// Files resolves all covered episodes from the actual manifest and merges the
// per-season choices. An unqualified filename cannot cover a mixed-season pack.
func Files(raw []byte, proof Proof) ([]torrentfiles.File, map[int][]int, error) {
	m, files, err := torrentmeta.Manifest(raw)
	if err != nil || m.InfoHash != proof.Hash || len(proof.Targets) == 0 {
		return nil, nil, ErrBatch
	}
	out := slices.Clone(files)
	for i := range out {
		out[i].Selected = false
	}
	covered := map[int][]int{}
	if len(proof.Targets) > 1001 {
		return nil, nil, ErrBatch
	}
	seen := map[int]bool{}
	for _, s := range proof.Targets {
		if s.Total < 1 || s.Total > 10000 || s.Number < 0 || s.Number > 1000 || seen[s.Number] {
			return nil, nil, ErrBatch
		}
		seen[s.Number] = true
		wanted, units, err := torrentfiles.PlanClassic(files, s.Number, s.Total, episodes(s), proof.Mixed)
		if errors.Is(err, torrentfiles.ErrNoCoverage) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		for i, f := range wanted {
			out[i].Selected = out[i].Selected || f.Selected
		}
		covered[s.Number] = units
	}
	if len(covered) == 0 {
		return nil, nil, torrentfiles.ErrNoCoverage
	}
	return out, covered, nil
}

// EpisodeLabel preserves holes and keeps long consecutive seasons readable.
func EpisodeLabel(covered map[int][]int) string {
	var parts []string
	for _, season := range slices.Sorted(maps.Keys(covered)) {
		var b strings.Builder
		fmt.Fprintf(&b, "S%02d", season)
		units := covered[season]
		for i := 0; i < len(units); {
			begin := i
			for i++; i < len(units) && units[i] == units[i-1]+1; i++ {
			}
			fmt.Fprintf(&b, "E%02d", units[begin])
			if i-begin > 1 {
				fmt.Fprintf(&b, "-E%02d", units[i-1])
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, " ")
}

func rank(in Input) []int {
	type row struct {
		index int
		key   string
	}
	rows := make([]row, 0, len(in.Candidates))
	for i, c := range in.Candidates {
		first, second := fmt.Sprintf("%03s", c.Site), fmt.Sprintf("%010s", c.Seeders)
		if in.Order == "seeder" {
			first, second = second, first
		}
		rows = append(rows, row{i, fmt.Sprintf("%-100s%03s%s%s%02d%04d", c.Title, c.Resource, first, second, len(c.Seasons), len(c.Episodes))})
	}
	slices.SortStableFunc(rows, func(a, b row) int { return cmp.Compare(b.key, a.key) })
	type identity struct {
		movie              bool
		show, title, units string
	}
	seen := map[identity]bool{}
	indices := []int{}
	for _, r := range rows {
		c := in.Candidates[r.index]
		key := identity{c.Movie, c.Show, c.Display, c.Label}
		if c.Movie {
			key.units = ""
		}
		if !seen[key] {
			seen[key] = true
			indices = append(indices, r.index)
		}
	}
	return indices
}
func targets(c Candidate, missing []Season, phase string) []Season {
	if c.Movie || len(c.Seasons) == 0 {
		return nil
	}
	result := []Season{}
	for _, s := range missing {
		if !slices.Contains(c.Seasons, s.Number) {
			continue
		}
		switch phase {
		case "whole":
			if len(c.Episodes) > 0 || len(s.Episodes) > 0 {
				return nil
			}
		case "episodes":
			if len(c.Seasons) != 1 || !subset(episodes(s), c.Episodes) {
				return nil
			}
		case "select":
			if len(c.Episodes) > 0 && len(s.Episodes) > 0 && !slices.ContainsFunc(c.Episodes, func(e int) bool { _, ok := slices.BinarySearch(s.Episodes, e); return ok }) {
				continue
			}
		}
		result = append(result, s)
	}
	if phase == "whole" && len(result) != len(c.Seasons) {
		return nil
	}
	return result
}
func subtract(missing []Season, covered map[int][]int) []Season {
	left := make([]Season, 0, len(missing))
	for _, s := range missing {
		if got := covered[s.Number]; len(got) > 0 {
			done := map[int]bool{}
			for _, n := range got {
				done[n] = true
			}
			s.Episodes = slices.DeleteFunc(slices.Clone(episodes(s)), func(n int) bool { return done[n] })
			if len(s.Episodes) == 0 {
				continue
			}
		}
		left = append(left, s)
	}
	return left
}
func Run(in Input, exchange func(Action) (reply, error)) (Result, error) {
	result := Result{"done", []int{}, in.Missing}
	for _, seasons := range in.Missing {
		seen := map[int]bool{}
		for _, s := range seasons {
			if s.Number < 0 || s.Number > 1000 || s.Total < 1 || s.Total > 10000 || seen[s.Number] {
				return result, ErrBatch
			}
			seen[s.Number] = true
			sort.Ints(s.Episodes)
			for i, n := range s.Episodes {
				if n < 1 || n > s.Total || i > 0 && n == s.Episodes[i-1] {
					return result, ErrBatch
				}
			}
		}
	}
	order := rank(in)
	attempted := map[int]bool{}
	type unit struct {
		show            string
		season, episode int
	}
	uncertain := map[unit]bool{}
	for _, phase := range []string{"movie", "whole", "episodes", "select"} {
		for _, i := range order {
			c := in.Candidates[i]
			if attempted[i] || c.Movie != (phase == "movie") {
				continue
			}
			needs := targets(c, in.Missing[c.Show], phase)
			if !c.Movie && len(needs) == 0 {
				continue
			}
			submit := Action{Op: "submit", Index: i}
			covered := map[int][]int{}
			switch phase {
			case "whole", "select":
				r, err := exchange(Action{Op: "inspect", Index: i})
				if err != nil {
					return result, err
				}
				if len(r.Torrent) == 0 {
					continue
				}
				m, err := torrentmeta.Parse(r.Torrent)
				if err != nil {
					continue
				}
				proof := Proof{Hash: m.InfoHash, Targets: needs, Mixed: len(c.Seasons) > 1, Whole: phase == "whole"}
				files, units, err := Files(r.Torrent, proof)
				if err != nil {
					continue
				}
				if phase == "whole" && !allCovered(needs, units) {
					continue
				}
				submit.Proof = &proof
				submit.Partial = RequiresSelection(proof, files, units)
				covered = units
			case "episodes":
				covered[c.Seasons[0]] = c.Episodes
			}
			scope := []unit{}
			if c.Movie {
				scope = append(scope, unit{c.Show + "|" + c.Display, -1, 0})
			}
			for season, units := range covered {
				for _, n := range units {
					scope = append(scope, unit{c.Show, season, n})
				}
			}
			attempted[i] = true
			if slices.ContainsFunc(scope, func(u unit) bool { return uncertain[u] }) {
				continue
			}
			r, err := exchange(submit)
			if err != nil {
				return result, err
			}
			// If acceptance is unknown, a different resource could repeat
			// the same episodes. Other shows and disjoint episodes can proceed.
			if r.Pending {
				for _, u := range scope {
					uncertain[u] = true
				}
				continue
			}
			if !r.Accepted {
				continue
			}
			result.Accepted = append(result.Accepted, i)
			if !c.Movie {
				left := subtract(in.Missing[c.Show], covered)
				if len(left) == 0 {
					delete(in.Missing, c.Show)
				} else {
					in.Missing[c.Show] = left
				}
			}
		}
	}
	return result, nil
}
func RequiresSelection(proof Proof, files []torrentfiles.File, units map[int][]int) bool {
	// A complete season requested as a whole may carry its original extras on
	// clients without file priorities. Any other media keeps the subset path.
	whole := proof.Whole && !slices.ContainsFunc(proof.Targets, func(s Season) bool { return len(s.Episodes) > 0 }) && allCovered(proof.Targets, units)
	return slices.ContainsFunc(files, func(f torrentfiles.File) bool {
		return !f.Padding && !f.Selected && (!whole || torrentfiles.ClassicVideo(f.Path))
	})
}

func allCovered(needs []Season, got map[int][]int) bool {
	for _, s := range needs {
		if !subset(got[s.Number], episodes(s)) {
			return false
		}
	}
	return true
}

// Each flushed action gets one response. No remote calls happen in the planner.
func Command(in io.Reader, out io.Writer) error {
	decoder := json.NewDecoder(in)
	decoder.DisallowUnknownFields()
	var input Input
	if err := decoder.Decode(&input); err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	result, err := Run(input, func(a Action) (reply, error) {
		var r reply
		if err := encoder.Encode(a); err != nil {
			return r, err
		}
		err := decoder.Decode(&r)
		return r, err
	})
	if err != nil {
		return err
	}
	return encoder.Encode(result)
}

// Intersect restricts subscription gaps to seasons still missing from the
// library. An empty episode array means an entire season in the classic API,
// so a disjoint intersection must remove the season.
func Intersect(in io.Reader, out io.Writer) error {
	var q struct{ Target, Library []Season }
	if json.NewDecoder(in).Decode(&q) != nil {
		return ErrBatch
	}
	missing := []Season{}
	for _, target := range q.Target {
		for _, lib := range q.Library {
			if lib.Number != target.Number {
				continue
			}
			if target.Total == 0 {
				target.Total = lib.Total
			}
			if len(lib.Episodes) == 0 {
				missing = append(missing, target)
				break
			}
			if len(target.Episodes) == 0 {
				target.Episodes = slices.Clone(lib.Episodes)
			} else {
				available := map[int]bool{}
				for _, n := range lib.Episodes {
					available[n] = true
				}
				target.Episodes = slices.DeleteFunc(target.Episodes, func(n int) bool { return !available[n] })
			}
			if len(target.Episodes) > 0 {
				sort.Ints(target.Episodes)
				target.Episodes = slices.Compact(target.Episodes)
				missing = append(missing, target)
			}
			break
		}
	}
	return json.NewEncoder(out).Encode(missing)
}
