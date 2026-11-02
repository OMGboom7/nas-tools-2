package sitesignin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func (s *session) execute(ctx context.Context) (Outcome, error) {
	switch Adapter(s.Target.URL) {
	case "carpt", "hdfans", "hdtime":
		return s.simple(ctx, "GET", "/attendance.php", nil, []string{"签到成功"}, []string{"请不要重复签到哦"})
	case "hdcity":
		return s.simple(ctx, "GET", "/sign", nil, []string{"本次签到获得魅力"}, []string{"已签到"})
	case "haidan":
		return s.simple(ctx, "GET", "/signin.php", nil, nil, []string{`value=["']已经打卡["']`})
	case "hhanclub":
		r, err := s.act(ctx, "GET", "/attendance.php", nil)
		if err != nil {
			return Outcome{}, err
		}
		page := r.text()
		if expired(page) {
			return failed("站点 Cookie 已失效", false), nil
		}
		if strings.Contains(page, "rule-tips") && matches(page, `今日签到排名：[[:space:]]*[0-9]+[[:space:]]*/[[:space:]]*[0-9]+`) {
			return signed("签到页已确认今日排名"), nil
		}
		return unknown(), nil
	case "hdarea":
		return s.simple(ctx, "POST", "/sign_in.php", url.Values{"action": {"sign_in"}}, []string{"此次签到您获得"}, []string{"请不要重复签到哦"})
	case "hares":
		return s.hares(ctx)
	case "pterclub":
		return s.pter(ctx)
	case "hdchina":
		return s.china(ctx)
	case "hdupt":
		return s.upt(ctx)
	case "ttg":
		return s.ttg(ctx)
	case "zhuque":
		return s.zhuque(ctx)
	case "btschool":
		return s.school(ctx)
	case "mteam":
		return s.mteam(ctx)
	case "hdsky", "opencd":
		return s.imageCaptcha(ctx, Adapter(s.Target.URL))
	case "u2":
		return s.u2(ctx)
	case "tjupt":
		return s.tjupt(ctx)
	case "52pt", "chdbits":
		return s.quiz(ctx, Adapter(s.Target.URL))
	default:
		return s.generic(ctx)
	}
}
func (s *session) simple(ctx context.Context, method, path string, data url.Values, success, repeat []string) (Outcome, error) {
	r, err := s.act(ctx, method, path, data)
	if err != nil {
		return Outcome{}, err
	}
	page := r.text()
	if expired(page) {
		return failed("站点 Cookie 已失效", false), nil
	}
	if Adapter(s.Target.URL)=="haidan" { if len(nodes(page,`input[value="已经打卡"]`))>0{return already(),nil};return unknown(),nil }
	visible:=pageText(page)
	if matches(visible, repeat...) {
		return already(), nil
	}
	if matches(visible, success...) {
		return signed("站点已确认签到成功"), nil
	}
	return unknown(), nil
}
func (s *session) home(ctx context.Context, path string) (string, error) {
	r, err := s.get(ctx, path)
	if err != nil {
		return "", err
	}
	page := r.text()
	if expired(page) {
		return "", ErrResponse
	}
	return page, nil
}
func (s *session) hares(ctx context.Context) (Outcome, error) {
	if _, err := s.home(ctx, "/"); err != nil {
		return Outcome{}, err
	}
	r, err := s.request(ctx, Exchange{Kind: "site", Method: "GET", URL: s.address("/attendance.php?action=sign"), Action: true}, "", http.Header{"Accept": {"application/json"}}, false)
	if err != nil {
		return Outcome{}, err
	}
	var v struct {
		Code    *int   `json:"code"`
		Message string `json:"msg"`
	}
	if json.Unmarshal(r.body, &v) != nil || v.Code == nil {
		return unknown(), nil
	}
	if *v.Code == 0 {
		return signed("站点已确认签到成功"), nil
	}
	if *v.Code == 1 && strings.Contains(v.Message, "今天已经签到") {
		return already(), nil
	}
	return unknown(), nil
}
func (s *session) pter(ctx context.Context) (Outcome, error) {
	r, err := s.act(ctx, "GET", "/attendance-ajax.php", nil)
	if err != nil {
		return Outcome{}, err
	}
	var v struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(r.body, &v) != nil {
		return unknown(), nil
	}
	if v.Status == "1" {
		return signed("站点已确认签到成功"), nil
	}
	if v.Status == "0" && strings.Contains(v.Message, "您今天已经签到过了") {
		return already(), nil
	}
	return unknown(), nil
}
func (s *session) china(ctx context.Context) (Outcome, error) {
	u, _ := url.Parse(s.origin + "/")
	// HDChina invalidates other session cookies daily. Retain exactly the
	// original hdchina cookie for the first page, then accept its new session.
	keep := []*http.Cookie{}
	for _, c := range s.jar.Cookies(u) {
		if c.Name == "hdchina" {
			keep = append(keep, c)
		}
	}
	if len(keep) != 1 {
		return failed("缺少原 hdchina Cookie", false), nil
	}
	jar, err := newSession(Executor{Target: Target{URL: s.Target.URL, Cookie: "hdchina=" + keep[0].Value}})
	if err != nil {
		return Outcome{}, err
	}
	s.jar = jar.jar
	page, err := s.home(ctx, "/index.php")
	if err != nil {
		return Outcome{}, err
	}
	if strings.Contains(page, "阻断页面") {
		return failed("站点返回阻断页面", false), nil
	}
	for _, a := range nodes(page, "a.label") {
		if strings.TrimSpace(nodeText(a)) == "已签到" {
			return already(), nil
		}
	}
	csrf := firstAttr(page, `meta[name="x-csrf"]`, "content")
	if csrf == "" {
		return Outcome{}, ErrResponse
	}
	r, err := s.act(ctx, "POST", "/plugin_sign-in.php?cmd=signin", url.Values{"csrf": {csrf}})
	if err != nil {
		return Outcome{}, err
	}
	var v struct {
		State string `json:"state"`
	}
	if json.Unmarshal(r.body, &v) == nil && v.State == "success" {
		return signed("站点已确认签到成功"), nil
	}
	return unknown(), nil
}
func (s *session) upt(ctx context.Context) (Outcome, error) {
	page, err := s.home(ctx, "/")
	if err != nil {
		return Outcome{}, err
	}
	if len(nodes(page, "span#yiqiandao")) > 0 {
		return already(), nil
	}
	r, err := s.act(ctx, "POST", "/added.php?action=qiandao", nil)
	if err != nil {
		return Outcome{}, err
	}
	if matches(strings.TrimSpace(r.text()), `^[0-9]*\.[0-9]+$`, `^[0-9]+$`) {
		return signed("站点已返回签到奖励数值"), nil
	}
	return unknown(), nil
}
func (s *session) ttg(ctx context.Context) (Outcome, error) {
	page, err := s.home(ctx, "/")
	if err != nil {
		return Outcome{}, err
	}
	if matches(page, `(?i)<b[^>]*style=["'][^"']*color:[[:space:]]*green[^"']*["'][^>]*>已签到</b>`) {
		return already(), nil
	}
	timestamp := capture(page, `signed_timestamp[[:space:]]*:[[:space:]]*["']?([0-9]{10})`)
	token := capture(page, `signed_token[[:space:]]*:[[:space:]]*["']([^"']+)["']`)
	if timestamp == "" || token == "" {
		return Outcome{}, ErrResponse
	}
	r, err := s.act(ctx, "POST", "/signed.php", url.Values{"signed_timestamp": {timestamp}, "signed_token": {token}})
	if err != nil {
		return Outcome{}, err
	}
	if strings.Contains(r.text(), "您已连续签到") {
		return signed("站点已确认连续签到"), nil
	}
	if strings.Contains(r.text(), "亲，您今天已签到过") {
		return already(), nil
	}
	return unknown(), nil
}
func (s *session) zhuque(ctx context.Context) (Outcome, error) {
	page, err := s.home(ctx, "/")
	if err != nil {
		return Outcome{}, err
	}
	csrf := firstAttr(page, `meta[name="x-csrf-token"]`, "content")
	if csrf == "" {
		return Outcome{}, ErrResponse
	}
	r, err := s.json(ctx, "/api/gaming/fireGenshinCharacterMagic", map[string]any{"all": 1, "resetModal": "true"}, http.Header{"X-Csrf-Token": {csrf}})
	if err != nil {
		return Outcome{}, err
	}
	var v struct {
		Status int `json:"status"`
		Data   struct {
			Code  string       `json:"code"`
			Bonus *json.Number `json:"bonus"`
		} `json:"data"`
	}
	if json.Unmarshal(r.body, &v) == nil && v.Status == 200 && v.Data.Code == "FIRE_GENSHIN_CHARACTER_MAGIC_SUCCESS" && v.Data.Bonus != nil {
		out := signed("朱雀已确认技能成功")
		return out, nil
	}
	out := unknown()
	out.Authenticated = authenticated(page)
	out.Reason = "登录与技能结果分别核对：未确认技能成功"
	return out, nil
}
func (s *session) school(ctx context.Context) (Outcome, error) {
	if s.Target.Browser {
		return s.browserCheckin(ctx, "/index.php", false)
	}
	page, err := s.home(ctx, "/index.php")
	if err != nil {
		return Outcome{}, err
	}
	if !strings.Contains(page, "魔力值") {
		return failed("未确认原站点登录状态", false), nil
	}
	if !strings.Contains(page, "每日签到") {
		return already(), nil
	}
	r, err := s.act(ctx, "GET", "/index.php?action=addbonus", nil)
	if err != nil {
		return Outcome{}, err
	}
	if authenticated(r.text()) && !strings.Contains(r.text(), "每日签到") {
		return signed("站点已确认每日签到完成"), nil
	}
	return unknown(), nil
}
func (s *session) u2(ctx context.Context) (Outcome, error) {
	if s.Now().In(s.Location).Hour() < 9 {
		return Outcome{State: "deferred", Reason: "U2 在本地时区 09:00 后签到", Retryable: true}, nil
	}
	page, err := s.home(ctx, "/showup.php")
	if err != nil {
		return Outcome{}, err
	}
	for _, n := range nodes(page, `a[href="showup.php"]`) {
		v := strings.TrimSpace(nodeText(n))
		if v == "已签到" || v == "Show Up" || v == "Показать" || v == "已簽到" {
			return already(), nil
		}
	}
	data := url.Values{"message": {"一切随缘~"}}
	for _, name := range []string{"req", "hash", "form"} {
		value := firstAttr(page, `form input[name="`+name+`"]`, "value")
		if value == "" {
			return Outcome{}, ErrResponse
		}
		data.Set(name, value)
	}
	buttons := nodes(page, `form input[type="submit"]`)
	if len(buttons) != 4 {
		return Outcome{}, ErrResponse
	}
	i, err := randomIndex(len(buttons))
	if err != nil {
		return Outcome{}, err
	}
	name, value := attr(buttons[i], "name"), attr(buttons[i], "value")
	if name == "" || value == "" {
		return Outcome{}, ErrResponse
	}
	data.Set(name, value)
	r, err := s.act(ctx, "POST", "/showup.php?action=show", data)
	if err != nil {
		return Outcome{}, err
	}
	if matches(r.text(), `window\.location\.href[[:space:]]*=[[:space:]]*['"]showup\.php['"]`) {
		return signed("U2 已确认签到提交成功"), nil
	}
	return unknown(), nil
}
