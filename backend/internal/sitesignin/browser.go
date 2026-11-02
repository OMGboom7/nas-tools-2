package sitesignin

import (
	"context"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
)

func isAlready(page []byte) bool {
	text := string(page)
	if !authenticated(text) {
		return false
	}
	return matches(pageText(text), `今日已签到|今天已经签到|今日已簽到|今天已经签过到了|请不要重复签到|签到已得\s*[0-9]+`)
}
func (s *session) browserCheckin(ctx context.Context, path string, login bool) (Outcome, error) {
	if s.Browser == nil || !s.Browser.Available() {
		return Outcome{State: "blocked", Reason: "原站点需要已配置的独立浏览器"}, nil
	}
	before := func(ctx context.Context, x browserruntime.Exchange) (func(string, string) error, error) {
		return s.Before(ctx, Exchange{Kind: "browser:" + x.Kind, Method: x.Method, URL: x.URL, Body: x.Body})
	}
	checkin := &browserruntime.Checkin{LoggedIn: func(page []byte) bool { return authenticated(string(page)) }, Already: isAlready, Completed: func(before, after []byte) bool {
		if !authenticated(string(after)) {
			return false
		}
		if Adapter(s.Target.URL) == "btschool" {
			return strings.Contains(string(before), "每日签到") && !strings.Contains(string(after), "每日签到")
		}
		return matches(pageText(string(after)), `签到成功|此次签到您获得|本次签到获得|签到已得\s*[0-9]+`) && !matches(pageText(string(before)), `签到成功|此次签到您获得|本次签到获得|签到已得\s*[0-9]+`)
	}, Action: func(ctx context.Context, kind string) (func(string, string) error, error) {
		return s.Before(ctx, Exchange{Kind: "action:" + kind, Method: "DOM", URL: s.address(path), Action: true})
	}}
	if Adapter(s.Target.URL) == "btschool" {
		checkin.Already = func(page []byte) bool {
			return authenticated(string(page)) && strings.Contains(string(page), "魔力值") && !strings.Contains(string(page), "每日签到")
		}
	}
	request := browserruntime.Request{URL: s.address(path), Cookie: s.Target.Cookie, UserAgent: s.Target.UserAgent, ReadOnly: false, Assets: true, Guard: s.Guard, Before: before, Transport: s.Transport, Checkin: checkin}
	if login && s.Options.SavedLogin {
		request.Login = s.Login
	}
	result, err := s.Browser.Render(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	switch result.CheckinState {
	case "signed":
		return signed("原站点页面已确认浏览器签到成功"), nil
	case "already":
		return already(), nil
	case "authenticated":
		return Outcome{State: "authenticated", Authenticated: true, Reason: "已确认登录有效，页面未提供明确签到结果"}, nil
	default:
		return unknown(), nil
	}
}
func (s *session) mteam(ctx context.Context) (Outcome, error) {
	if s.Browser == nil || !s.Browser.Available() {
		return Outcome{State: "blocked", Reason: "M-Team 需要独立浏览器"}, nil
	}
	request := browserruntime.Request{URL: s.Target.URL, Cookie: s.Target.Cookie, UserAgent: s.Target.UserAgent, ReadOnly: false, Assets: true, Guard: s.Guard, Transport: s.Transport, Before: func(ctx context.Context, x browserruntime.Exchange) (func(string, string) error, error) {
		return s.Before(ctx, Exchange{Kind: "browser:" + x.Kind, Method: x.Method, URL: x.URL, Body: x.Body, Action: x.Method == "POST"})
	}}
	if s.Options.SavedLogin && s.Login != nil {
		request.ReadOnly = false
		request.Login = s.Login
	}
	result, err := s.Browser.Render(ctx, request)
	if err != nil {
		return Outcome{}, err
	}
	if !authenticated(string(result.HTML)) {
		return failed("M-Team 未确认登录有效", false), nil
	}
	return Outcome{State: "authenticated", Authenticated: true, Reason: "M-Team 已确认访问及登录有效；没有把访问计作领取签到奖励"}, nil
}
func (s *session) generic(ctx context.Context) (Outcome, error) {
	if s.Target.Browser {
		path := "/"
		if strings.Contains(s.origin, "1ptba") {
			path = "/index.php"
		}
		return s.browserCheckin(ctx, path, false)
	}
	r, err := s.act(ctx, "GET", s.Target.URL, nil)
	if err != nil {
		return Outcome{}, err
	}
	page := r.text()
	if isAlready([]byte(page)) {
		return already(), nil
	}
	if !expired(page) && matches(pageText(page), `签到成功|此次签到您获得|本次签到获得`) {
		return signed("原签到地址已确认成功"), nil
	}
	if authenticated(page) {
		return Outcome{State: "authenticated", Authenticated: true, Reason: "原地址已确认登录有效；未确认签到奖励"}, nil
	}
	if expired(page) {
		return failed("站点 Cookie 已失效", false), nil
	}
	return unknown(), nil
}
