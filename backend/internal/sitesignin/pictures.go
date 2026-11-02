package sitesignin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/longbridgeapp/opencc"
)

func siteloginOCR(configuration map[string]any) ([]Receiver, error) {
	o, err := sitelogin.OCRConfig(configuration)
	if err != nil {
		return nil, err
	}
	out := []Receiver{}
	for _, r := range o.Receivers() {
		out = append(out, Receiver{r.Kind, r.Scheme, r.Host, r.Path, "验证码图片；百度模式还向原百度令牌接口提交原 API 凭据"})
	}
	return out, nil
}

// PictureHash uses the old 10x10 grayscale average hash. Bicubic resampling is
// separable and antialiased when reducing; grayscale follows Pillow's RGB->L
// coefficients, retaining compatibility with existing 100-character keys.
func PictureHash(raw []byte) (string, error) {
	if !validImage(raw) {
		return "", ErrResponse
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", ErrResponse
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	weights := func(length int) [][]float64 {
		out := make([][]float64, 10)
		scale := math.Max(1, float64(length)/10)
		for dst := 0; dst < 10; dst++ {
			out[dst] = make([]float64, length)
			center := (float64(dst) + 0.5) * float64(length) / 10
			sum := 0.0
			for p := int(math.Floor(center - 2*scale)); p < int(math.Ceil(center+2*scale)); p++ {
				if p < 0 || p >= length {
					continue
				}
				x := math.Abs((float64(p) + 0.5 - center) / scale)
				value := 0.0
				if x < 1 {
					value = ((1.5*x-2.5)*x)*x + 1
				} else if x < 2 {
					value = (((-0.5*x+2.5)*x - 4) * x) + 2
				}
				out[dst][p] = value
				sum += value
			}
			if sum != 0 {
				for p := range out[dst] {
					out[dst][p] /= sum
				}
			}
		}
		return out
	}
	xw, yw := weights(w), weights(h)
	clamp := func(v float64) uint8 { return uint8(math.Max(0, math.Min(255, math.Floor(v+0.5)))) }
	_, paletted := img.(*image.Paletted)
	monochrome := len(raw) > 25 && bytes.Equal(raw[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) && raw[24] == 1 && raw[25] == 0
	if paletted || monochrome {
		var gray [100]int
		sum := 0
		for y := 0; y < 10; y++ {
			for x := 0; x < 10; x++ {
				px := int((float64(x) + 0.5) * float64(w) / 10)
				py := int((float64(y) + 0.5) * float64(h) / 10)
				c := color.NRGBAModel.Convert(img.At(b.Min.X+px, b.Min.Y+py)).(color.NRGBA)
				v := (19595*int(c.R) + 38470*int(c.G) + 7471*int(c.B) + 32768) >> 16
				gray[y*10+x] = v
				sum += v
			}
		}
		var out strings.Builder
		for _, v := range gray {
			if v*100 > sum {
				out.WriteByte('1')
			} else {
				out.WriteByte('0')
			}
		}
		return out.String(), nil
	}
	// Retain only the ten horizontal samples for each input row.
	rows := make([][10]color.NRGBA, h)
	for y := 0; y < h; y++ {
		for x := 0; x < 10; x++ {
			r, g, blue := 0.0, 0.0, 0.0
			for px, weight := range xw[x] {
				if weight == 0 {
					continue
				}
				c := color.NRGBAModel.Convert(img.At(b.Min.X+px, b.Min.Y+y)).(color.NRGBA)
				r += float64(c.R) * weight
				g += float64(c.G) * weight
				blue += float64(c.B) * weight
			}
			rows[y][x] = color.NRGBA{clamp(r), clamp(g), clamp(blue), 255}
		}
	}
	var gray [100]int
	sum := 0
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			r, g, blue := 0.0, 0.0, 0.0
			for py, weight := range yw[y] {
				if weight == 0 {
					continue
				}
				c := rows[py][x]
				r += float64(c.R) * weight
				g += float64(c.G) * weight
				blue += float64(c.B) * weight
			}
			v := (19595*int(clamp(r)) + 38470*int(clamp(g)) + 7471*int(clamp(blue)) + 32768) >> 16
			gray[y*10+x] = v
			sum += v
		}
	}
	var result strings.Builder
	for _, v := range gray {
		if v*100 > sum {
			result.WriteByte('1')
		} else {
			result.WriteByte('0')
		}
	}
	return result.String(), nil
}
func similarity(a, b string) float64 {
	if len(a) != 100 || len(b) != 100 {
		return -1
	}
	same := 0
	for i := range a {
		if a[i] == b[i] {
			same++
		}
	}
	return float64(same) / 100
}

func (s *session) tjupt(ctx context.Context) (Outcome, error) {
	page, err := s.home(ctx, "/attendance.php")
	if err != nil {
		return Outcome{}, err
	}
	for _, a := range nodes(page, `a[href="attendance.php"]`) {
		if strings.TrimSpace(nodeText(a)) == "今日已签到" {
			return already(), nil
		}
	}
	imageURL := s.address(firstAttr(page, "table.captcha img", "src"))
	if imageURL == "" {
		return Outcome{}, ErrResponse
	}
	r, err := s.get(ctx, imageURL)
	if err != nil {
		return Outcome{}, err
	}
	hash, err := PictureHash(r.body)
	if err != nil {
		return Outcome{}, err
	}
	options := choices(page, `input[name="answer"]`)
	if len(options) == 0 {
		return Outcome{}, ErrResponse
	}
	cache, token, cacheErr := s.cached(ctx, "tjupt")
	answer := ""
	if values := cache[hash]; len(values) == 1 {
		for _, o := range options {
			if o.Value == values[0] {
				answer = o.Value
				break
			}
		}
		if answer == "" {
			for _, o := range options {
				if o.Text == values[0] {
					answer = o.Value
					break
				}
			}
		}
	}
	if answer == "" && s.Options.Douban {
		answer, err = s.doubanAnswer(ctx, hash, options)
		if err != nil {
			return Outcome{}, err
		}
	}
	if answer == "" && s.Options.Lens {
		answer, err = s.lensAnswer(ctx, imageURL, options)
		if err != nil {
			return Outcome{}, err
		}
	}
	if answer == "" {
		if s.Input == nil {
			return Outcome{}, ErrInput
		}
		values, err := s.Input(ctx, Challenge{Kind: "choice", MIME: http.DetectContentType(r.body), Image: r.body, Question: "选择原站点签到图片对应的答案", Choices: options})
		if err != nil {
			return Outcome{}, err
		}
		if len(values) != 1 || !validChoices(values, options) {
			return Outcome{}, ErrInput
		}
		answer = values[0]
	}
	r, err = s.act(ctx, "POST", "/attendance.php", url.Values{"answer": {answer}, "submit": {"提交"}})
	if err != nil {
		return Outcome{}, err
	}
	if matches(r.text(), `这是您的首次签到，本次签到获得[0-9]+个魔力值。`, `签到成功，这是您的第[0-9]+次签到，已连续签到[0-9]+天，本次签到获得[0-9]+个魔力值。`, `重新签到成功，本次签到获得[0-9]+个魔力值`) {
		out := signed("北洋已确认签到奖励")
		if cacheErr != nil {
			out.CacheState = "preserved"
			return out, nil
		}
		return s.cacheSuccess(ctx, out, "tjupt", token, hash, []string{answer}), nil
	}
	if matches(r.text(), `答案错误|回答错误|答错`) {
		return failed("站点明确返回图片答案错误", true), nil
	}
	return unknown(), nil
}
func (s *session) doubanAnswer(ctx context.Context, hash string, options []Choice) (string, error) {
	for i, o := range options {
		if o.Text == "" {
			continue
		}
		if i > 0 {
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		r, err := s.auxCall(ctx, "douban-question", "GET", "https://movie.douban.com/j/subject_suggest?"+url.Values{"q": {o.Text}}.Encode(), nil, "", nil)
		if err != nil {
			return "", err
		}
		var list []struct {
			Image string `json:"img"`
		}
		if json.Unmarshal(r.body, &list) != nil || len(list) > 30 {
			return "", ErrResponse
		}
		for _, item := range list {
			u, err := url.Parse(item.Image)
			if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
				return "", ErrScope
			}
			host := strings.ToLower(u.Hostname())
			if host != "doubanio.com" && !strings.HasSuffix(host, ".doubanio.com") && host != "douban.com" && !strings.HasSuffix(host, ".douban.com") {
				return "", ErrScope
			}
			r, err := s.auxCall(ctx, "douban-poster", "GET", item.Image, nil, "", nil)
			if err != nil {
				return "", err
			}
			other, err := PictureHash(r.body)
			if err != nil {
				return "", err
			}
			if similarity(hash, other) > 0.9 {
				return o.Value, nil
			}
		}
	}
	return "", nil
}
func (s *session) lensAnswer(ctx context.Context, imageURL string, options []Choice) (string, error) {
	if s.Browser == nil || !s.Browser.Available() {
		return "", ErrInput
	}
	address := "https://lens.google.com/uploadbyurl?" + url.Values{"url": {imageURL}}.Encode()
	r, err := s.Browser.Render(ctx, browserruntime.Request{URL: address, UserAgent: s.Target.UserAgent, ReadOnly: true, Assets: true, Transport: s.AuxTransport, Guard: s.Guard, ReadySelector: "div.UAiK1e", Before: func(ctx context.Context, x browserruntime.Exchange) (func(string, string) error, error) {
		return s.Before(ctx, Exchange{Kind: "lens:" + x.Kind, Method: x.Method, URL: x.URL, Body: x.Body})
	}})
	if err != nil {
		return "", err
	}
	results := nodes(string(r.HTML), "div.UAiK1e")
	if len(results) == 0 {
		return "", nil
	}
	if len(results) > 1000 {
		return "", ErrResponse
	}
	converter, err := opencc.New("t2s")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, n := range results {
		b.WriteString(nodeText(n))
		b.WriteByte('@')
	}
	all, err := converter.Convert(b.String())
	if err != nil {
		return "", err
	}
	threshold := len(results)
	if threshold > 3 {
		threshold = 3
	}
	best, bestCount := "", 0
	for _, o := range options {
		text := strings.TrimRight(o.Text, "0123456789")
		if text == "" {
			continue
		}
		count := strings.Count(all, text)
		if count >= threshold && count > bestCount {
			best, bestCount = o.Value, count
		}
	}
	return best, nil
}
