// Package downloaderhelper implements the original downloader assistant's
// task selection rules. Tracker URLs may contain passkeys; keep only the
// hostname keyword when handing observations to the scheduler.
package downloaderhelper

import (
	"errors"
	"net/url"
	"slices"
	"strings"
)

const Plugin = "DownloaderHelper"

var ErrSettings = errors.New("invalid downloader helper settings")

type Task struct {
	Hash, Name, Directory, Site string
	Added                       int64
	Labels                      []string
	CanSeed, Seeding, Missing   bool
}

func Keyword(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), ".")
	if len(parts) > 1 {
		return parts[len(parts)-2]
	}
	return parts[0]
}
func TagPair(keyword, name string, preferName bool) (want, drop string) {
	label := func(v string) string {
		if v == "" || len(v) > 248 || strings.ContainsAny(v, ",\x00\r\n") || strings.TrimSpace(v) != v {
			return ""
		}
		return "站点/" + v
	}
	want = label(keyword)
	if want == "" {
		return "", ""
	}
	drop = label(name)
	if preferName && drop != "" {
		want, drop = drop, want
	}
	if drop == want {
		drop = ""
	}
	return want, drop
}
func LabelsAfter(labels []string, want, drop string) []string {
	out := slices.Clone(labels)
	if drop != "" {
		out = slices.DeleteFunc(out, func(v string) bool { return v == drop })
	}
	if !slices.Contains(out, want) {
		out = append(out, want)
	}
	slices.Sort(out)
	return out
}
func Excluded(labels, exclude []string) bool {
	return slices.ContainsFunc(labels, func(v string) bool { return slices.Contains(exclude, v) })
}
