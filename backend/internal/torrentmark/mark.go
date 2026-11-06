package torrentmark

import (
	"errors"
	"net/url"
	"slices"
	"strings"
)

const (
	PT = "PT"
	BT = "BT"
)

var ErrEvidence = errors.New("种子证据缺失或已变化")

type Evidence struct {
	Hash     string
	Added    int64
	Labels   []string
	Trackers []string
	Private  *bool
	Complete bool
}

func Other(label string) string {
	if label == PT {
		return BT
	}
	return PT
}

func NormalizeLabels(labels []string) ([]string, error) {
	if labels == nil || len(labels) > 64 {
		return nil, ErrEvidence
	}
	out := slices.Clone(labels)
	for _, label := range out {
		if label == "" || len(label) > 256 || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\x00\r\n") {
			return nil, ErrEvidence
		}
	}
	slices.Sort(out)
	if len(slices.Compact(slices.Clone(out))) != len(out) {
		return nil, ErrEvidence
	}
	return out, nil
}

func Classify(e Evidence) string {
	if !e.Complete || e.Added <= 0 || len(e.Trackers) == 0 || len(e.Trackers) > 128 {
		return ""
	}
	cues := 0
	seen := map[string]bool{}
	for _, raw := range e.Trackers {
		if len(raw) > 4096 || strings.ContainsAny(raw, "\x00\r\n") {
			return ""
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "udp") || seen[raw] {
			return ""
		}
		seen[raw] = true
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return ""
		}
		if (u.Scheme == "http" || u.Scheme == "https") && (q.Get("secure") != "" || q.Get("passkey") != "" || strings.Contains(strings.ToLower(u.Hostname()), "totheglory")) {
			cues++
		}
	}
	if e.Private != nil {
		if *e.Private {
			return PT
		}
		if cues == 0 {
			return BT
		}
		return ""
	}
	if len(e.Trackers) == 1 && cues == 1 {
		return PT
	}
	if cues == 0 && !slices.Contains(e.Labels, PT) {
		return BT
	}
	return ""
}

func LabelsAfter(labels []string, want string) []string {
	next := slices.Clone(labels)
	next = slices.DeleteFunc(next, func(label string) bool { return label == Other(want) })
	if !slices.Contains(next, want) {
		next = append(next, want)
	}
	slices.Sort(next)
	return next
}
