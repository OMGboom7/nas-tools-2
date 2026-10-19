package subtitles

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, code int, body string) *http.Response {
	return &http.Response{Request: r, StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func testConfig(t *testing.T) Config {
	t.Helper()
	c, err := Parse(map[string]any{"host": "http://csf.local/base", "api_key": "private-key", "local_path": "/media", "remote_path": "/remote"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPathMappingTypesAndBluRayAPIShape(t *testing.T) {
	c := testConfig(t)
	for _, spec := range []struct {
		file, kind, want string
		video            int
	}{{"/media/Movie/Movie.mkv", "电影", "/remote/Movie/Movie.mkv", 0}, {"/media/Show/S01/E01.mkv", "电视剧", "/remote/Show/S01/E01.mkv", 1}, {"/media2/Movie.mkv", "动漫", "/media2/Movie.mkv", 1}} {
		p, err := c.Payload(spec.file, spec.kind)
		if err != nil || p.Path != spec.want || p.VideoType != spec.video || p.Priority != 3 || p.MediaID != "" || p.Bluray {
			t.Fatal(p, err)
		}
	}
	c.Mappings = append(c.Mappings, Mapping{"/remote", "D:/Library"})
	p, err := c.Payload("/media/Movie/Movie.mkv", "MOV")
	if err != nil || p.Path != "D:/Library/Movie/Movie.mkv" {
		t.Fatal(p, err)
	}
	p, err = c.BluRayPayload("/media/Movie (2026)")
	if err != nil || p.Path != "D:/Library/Movie (2026)/Movie (2026).mp4" || !p.Bluray {
		t.Fatal(p, err)
	}
	if _, err := c.Payload("/media/../private/Other.mkv", "电影"); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	if _, err := c.Payload("/media/a.mkv", "未知"); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
}

func TestSubmitOneAuthenticatedRequestAndNeverReplayAmbiguousResponse(t *testing.T) {
	for _, mode := range []string{"success", "lost", "redirect", "empty", "missing-id", "invalid-json", "401", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig(t)
			p, _ := c.Payload("/media/Movie.mkv", "电影")
			calls := 0
			transport := roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "http://csf.local/base/api/v1/add-job" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer private-key" {
					t.Fatal("wrong request scope", r.URL)
				}
				var body Payload
				if json.NewDecoder(r.Body).Decode(&body) != nil || body != p {
					t.Fatal("payload changed")
				}
				switch mode {
				case "lost":
					return nil, errors.New("private-key diagnostic")
				case "redirect":
					v := response(r, 302, "")
					v.Header.Set("Location", "https://attacker.test/")
					return v, nil
				case "empty":
					return response(r, 200, ""), nil
				case "missing-id":
					return response(r, 200, `{"message":"private-path invalid"}`), nil
				case "invalid-json":
					return response(r, 200, "garbage"), nil
				case "401":
					return response(r, 401, `{"message":"private-key"}`), nil
				case "oversize":
					return response(r, 200, strings.Repeat("x", (1<<20)+1)), nil
				default:
					return response(r, 200, `{"job_id":"Remote_Job-1","message":"ok"}`), nil
				}
			})
			id, err := Submit(t.Context(), c, p, transport)
			if calls != 1 {
				t.Fatal("retry", calls)
			}
			if mode == "success" {
				if err != nil || id != "Remote_Job-1" {
					t.Fatal(id, err)
				}
			} else if err == nil || id != "" || strings.Contains(err.Error(), "private-key") {
				t.Fatal(id, err)
			}
		})
	}
}

func TestInvalidConfigurationAndPayloadNeverContactService(t *testing.T) {
	for _, v := range []map[string]any{{"host": "http://user:secret@csf.local", "api_key": "key"}, {"host": "https://csf.local?token=secret", "api_key": "key"}, {"host": "http://csf.local", "api_key": "bad\r\nkey"}, {"host": "http://csf.local", "api_key": "key", "local_path": "/media"}, {"host": "http://csf.local", "api_key": "key", "local_path": "/media/../other", "remote_path": "/remote"}} {
		if _, err := Parse(v); err == nil {
			t.Fatal("accepted invalid config")
		}
	}
	c := testConfig(t)
	p, _ := c.Payload("/media/Movie.mkv", "电影")
	p.Bluray = true
	if _, err := Submit(t.Context(), c, p, roundTrip(func(r *http.Request) (*http.Response, error) { t.Fatal("invalid payload sent"); return nil, nil })); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
}
