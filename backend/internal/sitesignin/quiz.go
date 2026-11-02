package sitesignin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

type AIConfig struct{ URL, Key, Model string }

func QuestionAI(configuration map[string]any) (AIConfig, error) {
	o := AIConfig{URL: "https://api.openai.com/v1/chat/completions", Model: "gpt-3.5-turbo"}
	raw, ok := configuration["openai"]
	if !ok || raw == nil {
		return o, nil
	}
	v, ok := raw.(map[string]any)
	if !ok {
		return o, ErrScope
	}
	for key, dest := range map[string]*string{"api_key": &o.Key, "api_url": nil} {
		value, exists := v[key]
		if !exists || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || strings.ContainsAny(text, "\x00\r\n") {
			return o, ErrScope
		}
		if dest != nil {
			*dest = text
		} else if text != "" {
			o.URL = strings.TrimRight(text, "/") + "/v1/chat/completions"
		}
	}
	if browserruntime.Origin(o.URL) == "" || len(o.URL) > 16000 || len(o.Key) > 16000 {
		return o, ErrScope
	}
	return o, nil
}
func Helpers(configuration map[string]any, options Options) ([]Receiver, error) {
	out := []Receiver{}
	add := func(kind, address, data string) {
		u, _ := url.Parse(address)
		out = append(out, Receiver{kind, u.Scheme, u.Host, u.EscapedPath(), data})
	}
	if options.OCR {
		ocr, err := siteloginOCR(configuration)
		if err != nil {
			return nil, err
		}
		out = append(out, ocr...)
	}
	if options.Douban {
		add("豆瓣题目检索", "https://movie.douban.com/j/subject_suggest", "签到答案选项文字；后续公开海报来自豆瓣返回的图片地址，不携带站点凭据")
	}
	if options.Lens {
		add("Google Lens", "https://lens.google.com/uploadbyurl", "原站点验证码图片 URL；不携带站点 Cookie 或图片内容")
	}
	if options.AI {
		a, err := QuestionAI(configuration)
		if err != nil || a.Key == "" {
			return nil, ErrScope
		}
		add("原 OpenAI 兼容服务", a.URL, "题目、选项及原服务 API key；模型 "+a.Model)
	}
	return out, nil
}
func validChoices(values []string, options []Choice) bool {
	if len(values) == 0 || len(values) > len(options) {
		return false
	}
	allowed := map[string]bool{}
	for _, c := range options {
		allowed[c.Value] = true
	}
	seen := map[string]bool{}
	for _, v := range values {
		if !allowed[v] || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func (s *session) cached(ctx context.Context, name string) (map[string][]string, string, error) {
	if s.Cache == nil {
		return map[string][]string{}, "", nil
	}
	return s.Cache.Read(ctx, name)
}
func (s *session) cacheSuccess(ctx context.Context, out Outcome, name, token, key string, value []string) Outcome {
	if out.State != "signed" || s.Cache == nil {
		return out
	}
	if s.Guard(ctx) != nil {
		out.CacheState = "blocked"
		return out
	}
	if err := s.Cache.Put(ctx, name, token, key, value, s.Guard); err != nil {
		out.CacheState = "preserved"
	} else {
		out.CacheState = "saved"
	}
	return out
}
func (s *session) quiz(ctx context.Context, name string) (Outcome, error) {
	page, err := s.home(ctx, "/bakatest.php")
	if err != nil {
		return Outcome{}, err
	}
	if strings.Contains(page, "今天已经签过到了") {
		return already(), nil
	}
	id := firstAttr(page, `input[name="questionid"]`, "value")
	options := choices(page, `input[name="choice[]"]`)
	question := ""
	for _, n := range nodes(page, "td.text") {
		text := nodeText(n)
		if i := strings.Index(text, "请问："); i >= 0 {
			question = strings.TrimSpace(text[i+len("请问："):])
			break
		}
	}
	if id == "" || len(id) > 256 || len(options) == 0 || question == "" || len(question) > 12000 {
		return Outcome{}, ErrResponse
	}
	cache, token, cacheErr := s.cached(ctx, name)
	answer := cache[question]
	if !validChoices(answer, options) {
		if s.Options.AI {
			answer, err = s.aiAnswer(ctx, question, options)
			if err != nil {
				return Outcome{}, err
			}
		} else {
			i, e := randomIndex(len(options))
			if e != nil {
				return Outcome{}, e
			}
			answer = []string{options[i].Value}
		}
	}
	if !validChoices(answer, options) {
		return Outcome{}, ErrInput
	}
	r, err := s.act(ctx, "POST", "/bakatest.php", url.Values{"questionid": {id}, "choice[]": answer, "usercomment": {"太难了！"}, "wantskip": {"不会"}})
	if err != nil {
		return Outcome{}, err
	}
	if matches(r.text(), `[0-9]+点魔力值`) {
		out := signed("站点已确认答题奖励")
		if cacheErr != nil {
			out.CacheState = "preserved"
			return out, nil
		}
		return s.cacheSuccess(ctx, out, name, token, question, answer), nil
	}
	if strings.Contains(r.text(), "今天已经签过到了") {
		return already(), nil
	}
	if matches(r.text(), `回答错误|答案错误|答错`) {
		return failed("站点明确返回答案错误", true), nil
	}
	return unknown(), nil
}
func (s *session) aiAnswer(ctx context.Context, question string, options []Choice) ([]string, error) {
	a, err := QuestionAI(s.Configuration)
	if err != nil || a.Key == "" {
		return nil, ErrScope
	}
	var b strings.Builder
	b.WriteString("题目：" + question + "\n选项：{\n")
	for _, o := range options {
		fmt.Fprintf(&b, "%s:%s\n", o.Value, o.Text)
	}
	b.WriteString("}")
	payload, _ := json.Marshal(map[string]any{"model": a.Model, "user": "NAStool", "messages": []map[string]string{{"role": "system", "content": "下面我们来玩一个游戏，你是老师，我是学生，你需要回答我的问题，我会给你一个题目和几个选项，你的回复必须是给定选项中正确答案对应的序号，请直接回复数字"}, {"role": "user", "content": b.String()}}})
	r, err := s.auxCall(ctx, "question-ai", "POST", a.URL, payload, "application/json", http.Header{"Authorization": {"Bearer " + a.Key}})
	if err != nil {
		return nil, err
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(r.body, &v) != nil || len(v.Choices) != 1 || v.Choices[0].Finish != "stop" || len(v.Choices[0].Message.Content) > 4000 {
		return nil, ErrResponse
	}
	answer := regexp.MustCompile(`[0-9]+`).FindAllString(v.Choices[0].Message.Content, -1)
	if !validChoices(answer, options) {
		return nil, ErrInput
	}
	return answer, nil
}
