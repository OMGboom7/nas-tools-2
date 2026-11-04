package sitesignin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func validImage(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 2<<20 {
		return false
	}
	c, format, err := image.DecodeConfig(bytes.NewReader(raw))
	return err == nil && (format == "png" || format == "jpeg" || format == "gif") && c.Width > 0 && c.Height > 0 && c.Width <= 8192 && c.Height <= 8192 && c.Width*c.Height <= 16<<20
}
func (s *session) captcha(ctx context.Context, address string) (string, error) {
	r, err := s.get(ctx, address)
	if err != nil {
		return "", err
	}
	if !validImage(r.body) {
		return "", ErrResponse
	}
	if s.Options.OCR {
		ocr, err := sitelogin.OCRConfig(s.Configuration)
		if err != nil {
			return "", ErrScope
		}
		value, err := ocr.Read(ctx, r.body, s.AuxTransport, sitelogin.Guard(s.Guard), func(ctx context.Context, kind, method, address string, payload []byte) (func(string, string) error, error) {
			return s.Before(ctx, Exchange{Kind: kind, Method: method, URL: address, Body: payload})
		})
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if utf8.RuneCountInString(value) != 6 {
			return "", ErrInput
		}
		return value, nil
	}
	if s.Input == nil {
		return "", ErrInput
	}
	mime := http.DetectContentType(r.body)
	challenge := Challenge{Kind: "captcha", MIME: mime, Image: r.body, MinLength: 6, MaxLength: 6}
	values, err := s.Input(ctx, challenge)
	if err != nil {
		return "", err
	}
	if ValidateInput(challenge, values) != nil {
		return "", ErrInput
	}
	return values[0], nil
}
func (s *session) imageCaptcha(ctx context.Context, adapter string) (Outcome, error) {
	page, err := s.home(ctx, "/")
	if err != nil {
		return Outcome{}, err
	}
	if adapter == "hdsky" && strings.Contains(page, "已签到") || adapter == "opencd" && strings.Contains(page, "/plugin_sign-in.php?cmd=show-log") {
		return already(), nil
	}
	hash, imageURL, submit := "", "", "/plugin_sign-in.php?cmd=signin"
	data := url.Values{}
	if adapter == "hdsky" {
		r, err := s.call(ctx, "POST", "/image_code_ajax.php", url.Values{"action": {"new"}}, false)
		if err != nil {
			return Outcome{}, err
		}
		var v struct {
			Success bool   `json:"success"`
			Code    string `json:"code"`
		}
		if json.Unmarshal(r.body, &v) != nil || !v.Success || v.Code == "" || len(v.Code) > 512 {
			return Outcome{}, ErrResponse
		}
		hash = v.Code
		imageURL = "/image.php?" + url.Values{"action": {"regimage"}, "imagehash": {hash}}.Encode()
		submit = "/showup.php"
		data.Set("action", "showup")
	} else {
		page, err = s.home(ctx, "/plugin_sign-in.php")
		if err != nil {
			return Outcome{}, err
		}
		hash = firstAttr(page, `form#frmSignin input[name="imagehash"]`, "value")
		imageURL = firstAttr(page, "form#frmSignin img", "src")
		if hash == "" || imageURL == "" {
			return Outcome{}, ErrResponse
		}
	}
	code, err := s.captcha(ctx, imageURL)
	if err != nil {
		return Outcome{}, err
	}
	data.Set("imagehash", hash)
	data.Set("imagestring", code)
	r, err := s.act(ctx, "POST", submit, data)
	if err != nil {
		return Outcome{}, err
	}
	if adapter == "hdsky" {
		var v struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if json.Unmarshal(r.body, &v) != nil {
			return unknown(), nil
		}
		if v.Success {
			return signed("站点已确认验证码签到成功"), nil
		}
		if v.Message == "date_unmatch" {
			return already(), nil
		}
		if v.Message == "invalid_imagehash" {
			return failed("站点确认验证码失效", true), nil
		}
	} else {
		var v struct {
			State string `json:"state"`
		}
		if json.Unmarshal(r.body, &v) == nil && v.State == "success" {
			return signed("站点已确认验证码签到成功"), nil
		}
	}
	return unknown(), nil
}
