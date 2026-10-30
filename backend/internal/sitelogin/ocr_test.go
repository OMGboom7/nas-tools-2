package sitelogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type ocrTransport func(*http.Request) (*http.Response, error)

func (f ocrTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func ocrResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func TestOriginalOCRProtocolsAndExplicitFallback(t *testing.T) {
	for _, mode := range []string{"baidu", "empty-baidu", "custom", "lost", "denied"} {
		t.Run(mode, func(t *testing.T) {
			calls, steps, finished := 0, 0, 0
			last := ""
			config := OCR{CustomURL: "https://ocr.fixture.test/captcha"}
			if mode != "custom" {
				config.APIKey, config.SecretKey = "fixture-key", "fixture-secret"
			}
			transport := ocrTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "POST" || r.GetBody != nil || !r.Close || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("OCR request scope or no-replay transport lost")
				}
				if r.URL.Host == "aip.baidubce.com" {
					if r.URL.Path == "/oauth/2.0/token" {
						if r.URL.Query().Get("client_id") != "fixture-key" || r.URL.Query().Get("client_secret") != "fixture-secret" {
							t.Error("original token shape changed")
						}
						out := ocrResponse(r, `{"access_token":"fixture-access-token"}`)
						if mode == "denied" {
							out.StatusCode = 401
						}
						return out, nil
					}
					if r.URL.Path != "/rest/2.0/ocr/v1/accurate_basic" || r.URL.Query().Get("access_token") != "fixture-access-token" || r.ParseForm() != nil || r.PostForm.Get("image") != base64.StdEncoding.EncodeToString([]byte("fixture-image")) {
						t.Error("original OCR payload changed")
					}
					if mode == "lost" {
						return nil, io.ErrUnexpectedEOF
					}
					if mode == "empty-baidu" {
						return ocrResponse(r, `{"words_result":[]}`), nil
					}
					return ocrResponse(r, `{"words_result":[{"words":"1234"}]}`), nil
				}
				if r.URL.String() != "https://ocr.fixture.test/captcha" {
					t.Error("OCR destination changed")
				}
				var payload map[string]string
				if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["base64_img"] != base64.StdEncoding.EncodeToString([]byte("fixture-image")) {
					t.Error("original server OCR shape changed")
				}
				return ocrResponse(r, `{"result":"5678"}`), nil
			})
			before := func(context.Context, string, string, string, []byte) (func(string, string) error, error) {
				steps++
				return func(state, digest string) error { finished++; last = state; return nil }, nil
			}
			got, err := config.Read(t.Context(), []byte("fixture-image"), transport, good, before)
			switch mode {
			case "baidu":
				if err != nil || got != "1234" || calls != 2 {
					t.Fatal(got, err, calls)
				}
			case "empty-baidu":
				if err != nil || got != "5678" || calls != 3 {
					t.Fatal(got, err, calls)
				}
			case "custom":
				if err != nil || got != "5678" || calls != 1 {
					t.Fatal(got, err, calls)
				}
			case "lost":
				if err == nil || calls != 2 || last != "uncertain" {
					t.Fatal("lost OCR automatically fell back or retried", err, calls, last)
				}
			case "denied":
				if err == nil || calls != 1 || last != "denied" {
					t.Fatal("denied OCR continued", err, calls, last)
				}
			}
			if steps != calls || finished != steps {
				t.Fatal("actual requests not journaled", steps, calls, finished)
			}
		})
	}
}
func TestOCRRejectsScopeOrIntentFailureBeforeNetwork(t *testing.T) {
	for _, address := range []string{"ftp://public.example/ocr", "https://user:password@ocr.test", "file:///tmp/image", "https://ocr.test/#fragment"} {
		if _, err := OCRConfig(map[string]any{"ocr": map[string]any{"custom_ocr_url": address}}); err == nil {
			t.Fatal("unsafe OCR endpoint accepted", address)
		}
	}
	calls := 0
	tr := ocrTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })
	_, err := (OCR{CustomURL: "https://ocr.test/"}).Read(t.Context(), []byte("fixture"), tr, good, func(context.Context, string, string, string, []byte) (func(string, string) error, error) {
		return nil, ErrState
	})
	if err == nil || calls != 0 {
		t.Fatal("unrecorded OCR request", err, calls)
	}
}

func TestOriginalConfiguredHTTPOCRKeepsReceiverAndDoesNotForwardSiteSecrets(t *testing.T) {
	address := "http://ocr-fixture.internal:9898/captcha/base64?fixture_key=private"
	o, err := OCRConfig(map[string]any{"ocr": map[string]any{"custom_ocr_url": address}})
	if err != nil {
		t.Fatal("original HTTP OCR was rejected", err)
	}
	receivers := o.Receivers()
	if len(receivers) != 1 || receivers[0].Scheme != "http" || receivers[0].Host != "ocr-fixture.internal:9898" || receivers[0].Path != "/captcha/base64" {
		t.Fatal("original OCR receiver changed", receivers)
	}
	calls := 0
	transport := ocrTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != address || r.Method != "POST" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("original OCR request scope changed")
		}
		var payload map[string]string
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload["base64_img"] != base64.StdEncoding.EncodeToString([]byte("fixture-image")) {
			t.Error("original OCR image payload changed")
		}
		return ocrResponse(r, `{"result":"1357"}`), nil
	})
	got, err := o.Read(t.Context(), []byte("fixture-image"), transport, good, func(context.Context, string, string, string, []byte) (func(string, string) error, error) {
		return func(string, string) error { return nil }, nil
	})
	if err != nil || got != "1357" || calls != 1 {
		t.Fatal("configured HTTP OCR failed or repeated", got, err, calls)
	}
}
