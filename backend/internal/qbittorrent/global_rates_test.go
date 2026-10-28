package qbittorrent

import (
	"context"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestGlobalRatesUseActiveTransferBytesAndOneSettingPerWrite(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		t.Run(mode, func(t *testing.T) {
			writes, logins := 0, 0
			upload, download := int64(0), int64(512)
			lost := false
			client, err := New("https://global-q.test/prefix", "", "user", "private-password", transportFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/prefix/api/v2/auth/login":
					logins++
					return reply(200, "Ok."), nil
				case "/prefix/api/v2/transfer/speedLimitsMode":
					return reply(200, mode), nil
				case "/prefix/api/v2/transfer/uploadLimit":
					return reply(200, strconv.FormatInt(upload, 10)), nil
				case "/prefix/api/v2/transfer/downloadLimit":
					return reply(200, strconv.FormatInt(download, 10)), nil
				case "/prefix/api/v2/transfer/setUploadLimit", "/prefix/api/v2/transfer/setDownloadLimit":
					writes++
					if r.Method != "POST" || r.ParseForm() != nil || len(r.Form) != 1 {
						t.Fatal("wrong mutation", r.URL, r.Form)
					}
					n, err := strconv.ParseInt(r.Form.Get("limit"), 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasSuffix(r.URL.Path, "setUploadLimit") {
						upload = n
					} else {
						download = n
					}
					if lost {
						return nil, errors.New("lost after acceptance")
					}
					return reply(200, ""), nil
				default:
					t.Fatal("wrong global API", r.URL)
					return nil, context.Canceled
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			s, before, err := client.OpenGlobalRates(t.Context())
			if err != nil || before.Upload != 0 || before.Download != 512 || before.Alternative != (mode == "1") {
				t.Fatal(before, err)
			}
			after, err := before.Desired("qbittorrent", 32, 64)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Write(t.Context(), "upload", after); err != nil {
				t.Fatal(err)
			}
			got, err := s.Read(t.Context())
			if err != nil || got.Upload != 65536 || got.Download != 512 {
				t.Fatal(got, err)
			}
			lost = true
			if err := s.Write(t.Context(), "download", after); err == nil {
				t.Fatal("lost ack accepted")
			}
			if writes != 2 || logins != 1 || download != 32768 {
				t.Fatal(writes, logins, download)
			}
			if err := s.Write(t.Context(), "pair", after); err == nil || writes != 2 {
				t.Fatal("unsupported dimension wrote")
			}
		})
	}
}
func TestGlobalRatesFailClosedOnModeChangesAndIncompleteNumbers(t *testing.T) {
	for _, value := range []string{"-1", "1.1", "null", "{}", "1099511627777"} {
		t.Run(value, func(t *testing.T) {
			client, _ := New("q.test", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "login") {
					return reply(200, "Ok."), nil
				}
				if strings.HasSuffix(r.URL.Path, "speedLimitsMode") {
					return reply(200, "0"), nil
				}
				return reply(200, value), nil
			}))
			if _, _, err := client.OpenGlobalRates(t.Context()); err == nil {
				t.Fatal("bad current global rate", value)
			}
		})
	}
	calls := 0
	client, _ := New("q.test", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "login") {
			return reply(200, "Ok."), nil
		}
		if strings.HasSuffix(r.URL.Path, "speedLimitsMode") {
			calls++
			return reply(200, strconv.Itoa(calls-1)), nil
		}
		return reply(200, "0"), nil
	}))
	if _, _, err := client.OpenGlobalRates(t.Context()); err == nil {
		t.Fatal("mode changed inside snapshot")
	}
}
func TestGlobalRateMutationRefusesRedirectAndDoesNotRelogin(t *testing.T) {
	for _, code := range []int{302, 307, 403, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls, logins := 0, 0
			client, _ := New("q.test", "", "", "", transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "q.test" {
					t.Fatal("redirect followed")
				}
				if strings.HasSuffix(r.URL.Path, "login") {
					logins++
					return reply(200, "Ok."), nil
				}
				if strings.Contains(r.URL.Path, "setUploadLimit") {
					calls++
					p := reply(code, "private error")
					p.Header.Set("Location", "https://evil.test")
					return p, nil
				}
				return reply(200, "0"), nil
			}))
			s, _, err := client.OpenGlobalRates(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Write(t.Context(), "upload", speedlimit.Rates{Unit: "B/s", Upload: 1024, UploadLimited: true}); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal(err)
			}
			if calls != 1 || logins != 1 {
				t.Fatal(calls, logins)
			}
		})
	}
}
