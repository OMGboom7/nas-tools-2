package browserruntime

import (
	"context"
	"errors"
)

var ErrLogin = errors.New("site login did not establish the original site's session")
var ErrInput = errors.New("site login input expired or is not available")

// Login is an explicit original-site workflow. Secrets and page images stay
// in memory; durable callers journal only salted operation signatures/receipts.
type Login struct {
	Username, Password, TwoStep string
	Interact                    func(context.Context, LoginChallenge) (string, error)
	LoggedIn                    func([]byte) bool
	Action                      func(context.Context, string) (func(string, string) error, error)
}
type LoginChallenge struct {
	Kind, MIME string
	Image      []byte
}

func (l *Login) valid() bool {
	return l != nil && l.Username != "" && l.Password != "" && text(l.Username, 1024) && text(l.Password, 4096) && text(l.TwoStep, 128) && l.Interact != nil && l.LoggedIn != nil && l.Action != nil
}

// These are the original SiteConf login XPath rules, with visible-field
// selection so a hidden legacy form cannot receive credentials accidentally.
var loginRules = map[string][]string{
	"username":    {`//input[@name="username"]`, `//input[@id="form_item_username"]`, `//input[@id="username"]`},
	"password":    {`//input[@name="password"]`, `//input[@id="form_item_password"]`, `//input[@id="password"]`},
	"captcha":     {`//input[@name="imagestring"]`, `//input[@name="captcha"]`, `//input[@id="form_item_captcha"]`},
	"captcha_img": {`//img[@alt="CAPTCHA"]/@src`, `//img[@alt="SECURITY CODE"]/@src`, `//img[@id="LAY-user-get-vercode"]/@src`, `//img[contains(@src,"/api/getCaptcha")]/@src`},
	"submit":      {`//input[@type="submit"]`, `//button[@type="submit"]`, `//button[@lay-filter="login"]`, `//button[@lay-filter="formLogin"]`, `//input[@type="button"][@value="登录"]`},
	"twostep":     {`//input[@name="two_step_code"]`, `//input[@name="2fa_secret"]`},
}
