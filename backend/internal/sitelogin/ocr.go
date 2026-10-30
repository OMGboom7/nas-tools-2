package sitelogin

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrOCR = errors.New("original OCR service did not return a usable code")

type OCR struct{ CustomURL, APIKey, SecretKey string }
type OCRReceiver struct {
	Kind   string `json:"kind"`
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Path   string `json:"path"`
}
type OCRBefore func(context.Context, string, string, string, []byte) (func(string, string) error, error)

func OCRConfig(configuration map[string]any) (OCR, error) {
	out := OCR{CustomURL: "https://ocr.ddsrem.com/captcha/base64"}
	raw, ok := configuration["ocr"]
	if !ok || raw == nil {
		return out, nil
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return out, ErrScope
	}
	for key, target := range map[string]*string{"custom_ocr_url": &out.CustomURL, "baiduocr_api_key": &out.APIKey, "baiduocr_secret_key": &out.SecretKey} {
		if value, exists := values[key]; exists && value != nil {
			str, ok := value.(string)
			if !ok || !Text(str, 16000) {
				return out, ErrScope
			}
			if key != "custom_ocr_url" || str != "" {
				*target = str
			}
		}
	}
	out.CustomURL = strings.TrimRight(out.CustomURL, "/")
	if _, err := ocrURL(out.CustomURL); err != nil {
		return out, err
	}
	return out, nil
}
func ocrURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" || !Text(raw, 16000) {
		return nil, ErrScope
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, ErrScope
	}
	return u, nil
}
func (o OCR) Receivers() []OCRReceiver {
	out := []OCRReceiver{}
	if o.APIKey != "" && o.SecretKey != "" {
		out = append(out, OCRReceiver{Kind: "百度 OCR", Scheme: "https", Host: "aip.baidubce.com", Path: "/rest/2.0/ocr/v1/accurate_basic"})
	}
	if u, err := ocrURL(o.CustomURL); err == nil {
		out = append(out, OCRReceiver{Kind: "原服务端 OCR", Scheme: u.Scheme, Host: u.Host, Path: u.EscapedPath()})
	}
	return out
}
func (o OCR) Read(ctx context.Context, image []byte, transport http.RoundTripper, guard Guard, before OCRBefore) (string, error) {
	if len(image) == 0 || len(image) > 2<<20 || guard == nil || before == nil {
		return "", ErrScope
	}
	encoded := base64.StdEncoding.EncodeToString(image)
	if o.APIKey != "" && o.SecretKey != "" {
		tokenURL := "https://aip.baidubce.com/oauth/2.0/token?" + url.Values{"grant_type": {"client_credentials"}, "client_id": {o.APIKey}, "client_secret": {o.SecretKey}}.Encode()
		raw, status, err := ocrCall(ctx, "ocr-token", tokenURL, nil, "", transport, guard, before)
		if err != nil {
			return "", err
		}
		var token struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		if json.Unmarshal(raw, &token) != nil {
			return "", ErrOCR
		}
		if status == 200 && Text(token.AccessToken, 16000) && token.AccessToken != "" {
			address := "https://aip.baidubce.com/rest/2.0/ocr/v1/accurate_basic?" + url.Values{"access_token": {token.AccessToken}}.Encode()
			payload := []byte(url.Values{"image": {encoded}}.Encode())
			raw, status, err = ocrCall(ctx, "ocr-image", address, payload, "application/x-www-form-urlencoded", transport, guard, before)
			if err != nil {
				return "", err
			}
			var result struct {
				Words []struct {
					Words string `json:"words"`
				} `json:"words_result"`
				ErrorCode *int `json:"error_code"`
			}
			if json.Unmarshal(raw, &result) != nil || len(result.Words) > 64 {
				return "", ErrOCR
			}
			parts := []string{}
			for _, word := range result.Words {
				if !Text(word.Words, 128) {
					return "", ErrOCR
				}
				parts = append(parts, word.Words)
			}
			code := strings.TrimSpace(strings.Join(parts, " "))
			if status == 200 && Text(code, 128) && code != "" {
				return code, nil
			}
			// A complete, parsed empty/error response can use the original fallback.
			// An unknown response above always stops, rather than spending again.
		} else if token.Error == "" {
			return "", ErrOCR
		}
	}
	payload, _ := json.Marshal(map[string]string{"base64_img": encoded})
	raw, status, err := ocrCall(ctx, "ocr-image", o.CustomURL, payload, "application/json", transport, guard, before)
	if err != nil {
		return "", err
	}
	var result struct {
		Result string `json:"result"`
	}
	if status != 200 || json.Unmarshal(raw, &result) != nil || !Text(result.Result, 128) || strings.TrimSpace(result.Result) == "" {
		return "", ErrOCR
	}
	return strings.TrimSpace(result.Result), nil
}
func ocrCall(ctx context.Context, kind, address string, payload []byte, contentType string, transport http.RoundTripper, guard Guard, before OCRBefore) ([]byte, int, error) {
	if _, err := ocrURL(address); err != nil {
		return nil, 0, err
	}
	if err := guard(ctx); err != nil {
		return nil, 0, ErrScope
	}
	finish, err := before(ctx, kind, "POST", address, payload)
	if err != nil || finish == nil {
		return nil, 0, ErrState
	}
	request, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(payload))
	if err != nil {
		_ = finish("uncertain", "")
		return nil, 0, ErrOCR
	}
	request.GetBody = nil
	request.Close = true
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		clone.MaxResponseHeaderBytes = 64 << 10
		clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = clone
	}
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		_ = finish("uncertain", "")
		return nil, 0, ErrOCR
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		_ = finish("uncertain", "")
		return nil, 0, ErrOCR
	}
	state := "received"
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 406 || response.StatusCode == 429 {
		state = "denied"
	}
	if err = finish(state, Hash([]any{response.StatusCode, raw})); err != nil {
		return nil, 0, ErrState
	}
	if state == "denied" || guard(ctx) != nil {
		return nil, 0, ErrScope
	}
	return raw, response.StatusCode, nil
}
