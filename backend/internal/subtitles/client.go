package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
)

var ErrConfig = errors.New("invalid ChineseSubFinder configuration")
var ErrResponse = errors.New("ChineseSubFinder submission not positively acknowledged")

const Plugin = "ChineseSubFinder"
const ConfigKey = "plugin." + Plugin
const EpochKey = "GoSubtitlePluginEpoch"

type Mapping struct{ Local, Remote string }
type Config struct {
	Host, Key string
	Mappings  []Mapping
}
type Payload struct {
	VideoType int    `json:"video_type"`
	Path      string `json:"physical_video_file_full_path"`
	Priority  int    `json:"task_priority_level"`
	MediaID   string `json:"media_server_inside_video_id"`
	Bluray    bool   `json:"is_bluray"`
}

func endpoint(host string) (string, error) {
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	u, err := url.Parse(host)
	if err != nil || len(host) > 2048 || strings.ContainsFunc(host, unicode.IsControl) || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Scheme != "http" && u.Scheme != "https" {
		return "", ErrConfig
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/add-job"
	u.RawPath = ""
	return u.String(), nil
}

func portablePath(s string) (string, error) {
	s = strings.ReplaceAll(s, `\`, "/")
	if len(s) > 4096 || strings.ContainsFunc(s, unicode.IsControl) || s == "" {
		return "", ErrConfig
	}
	drive := ""
	rest := s
	if len(s) > 2 && s[1] == ':' && s[2] == '/' && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) {
		drive = s[:2]
		rest = s[2:]
	} else if !strings.HasPrefix(s, "/") {
		return "", ErrConfig
	}
	for _, part := range strings.Split(rest, "/") {
		if part == ".." || part == "." {
			return "", ErrConfig
		}
	}
	unc := drive == "" && strings.HasPrefix(rest, "//")
	rest = path.Clean(rest)
	if unc {
		rest = "/" + rest
	}
	return drive + rest, nil
}

func Parse(values map[string]any) (Config, error) {
	c := Config{}
	for key, target := range map[string]*string{"host": &c.Host, "api_key": &c.Key} {
		v, ok := values[key].(string)
		if !ok {
			return c, ErrConfig
		}
		*target = v
	}
	if _, err := endpoint(c.Host); err != nil || c.Key == "" || len(c.Key) > 4096 || strings.ContainsFunc(c.Key, unicode.IsControl) {
		return c, ErrConfig
	}
	for _, suffix := range []string{"", "2", "3"} {
		local, remote := "", ""
		for key, target := range map[string]*string{"local_path" + suffix: &local, "remote_path" + suffix: &remote} {
			if raw, exists := values[key]; exists {
				v, ok := raw.(string)
				if !ok {
					return c, ErrConfig
				}
				*target = v
			}
		}
		if local == "" && remote == "" {
			continue
		}
		if local == "" || remote == "" {
			return c, ErrConfig
		}
		var err error
		local, err = portablePath(local)
		if err != nil {
			return c, err
		}
		remote, err = portablePath(remote)
		if err != nil {
			return c, err
		}
		c.Mappings = append(c.Mappings, Mapping{local, remote})
	}
	return c, nil
}

// Preserve ordered mappings while replacing only one complete root prefix,
// never all matching substrings or /media2 when the rule covers /media.
func (c Config) Payload(file, mediaType string) (Payload, error) {
	file, err := portablePath(file)
	if err != nil {
		return Payload{}, err
	}
	typeID := 1
	switch mediaType {
	case "电影", "MOV":
		typeID = 0
	case "电视剧", "动漫", "TV", "ANI":
	default:
		return Payload{}, ErrConfig
	}
	for _, m := range c.Mappings {
		root := strings.TrimRight(m.Local, "/")
		if file == m.Local || strings.HasPrefix(file, root+"/") {
			file = strings.TrimRight(m.Remote, "/") + strings.TrimPrefix(file, root)
		}
	}
	file, err = portablePath(file)
	if err != nil {
		return Payload{}, err
	}
	return Payload{VideoType: typeID, Path: file, Priority: 3, MediaID: "", Bluray: false}, nil
}

// The CSF API represents a Blu-ray movie directory with a virtual .mp4 whose
// basename matches that directory. Producers must independently prove the
// Blu-ray source; ordinary Go intake events never infer or use this mode.
func (c Config) BluRayPayload(directory string) (Payload, error) {
	p, err := c.Payload(directory, "电影")
	if err != nil {
		return p, err
	}
	name := path.Base(p.Path)
	if name == "/" || name == "." {
		return p, ErrConfig
	}
	p.Path = strings.TrimRight(p.Path, "/") + "/" + name + ".mp4"
	p.Bluray = true
	return p, nil
}

func ValidID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Submit performs exactly one POST. Missing job_id, HTTP failures, redirects and
// transport errors are ambiguous; no refresh, alternate endpoint or replay.
func Submit(ctx context.Context, c Config, p Payload, transport http.RoundTripper) (string, error) {
	address, err := endpoint(c.Host)
	if err != nil || c.Key == "" || len(c.Key) > 4096 || strings.ContainsFunc(c.Key, unicode.IsControl) {
		return "", ErrConfig
	}
	if _, err := portablePath(p.Path); err != nil || p.VideoType < 0 || p.VideoType > 1 || p.Priority != 3 || p.MediaID != "" {
		return "", ErrConfig
	}
	if p.Bluray && (p.VideoType != 0 || path.Base(p.Path) != path.Base(path.Dir(p.Path))+".mp4") {
		return "", ErrConfig
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", ErrConfig
	}
	r, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(b))
	if err != nil {
		return "", ErrConfig
	}
	r.Header.Set("Authorization", "Bearer "+c.Key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		return "", ErrResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", ErrResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", ErrResponse
	}
	var result struct {
		ID string `json:"job_id"`
	}
	if json.Unmarshal(data, &result) != nil || !ValidID(result.ID) {
		return "", ErrResponse
	}
	return result.ID, nil
}
