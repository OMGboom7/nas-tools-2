package browserruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chromedp/chromedp"
)

// Checkin uses only the original built-in SiteConf selectors. The caller must
// independently recognize authenticated, already-signed and completed pages.
type Checkin struct {
	LoggedIn  func([]byte) bool
	Already   func([]byte) bool
	Completed func(before, after []byte) bool
	Action    func(context.Context, string) (func(string, string) error, error)
}

func (c *Checkin) valid() bool {
	return c != nil && c.LoggedIn != nil && c.Already != nil && c.Completed != nil && c.Action != nil
}

var checkinRules = []string{
	`//a[@id="signed"]`,
	`//a[contains(@href,"attendance")]`,
	`//a[contains(text(),"签到")]`,
	`//a/b[contains(text(),"签 到")]`,
	`//span[@id="sign_in"]/a`,
	`//a[contains(@href,"addbonus")]`,
	`//input[@class="dt_button"][contains(@value,"打卡")]`,
	`//a[contains(@href,"sign_in")]`,
	`//a[contains(@onclick,"do_signin")]`,
	`//a[@id="do-attendance"]`,
	`//shark-icon-button[@href="attendance.php"]`,
}

func (b *broker) checkin(ctx context.Context, c *Checkin) (string, error) {
	if err := loginDelay(ctx, 3*time.Second); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	var before []byte
	paths, _ := json.Marshal(checkinRules)
	expression := `(()=>{const paths=` + string(paths) + `;for(const p of paths){const n=document.evaluate(p,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;if(!n)continue;const e=n.tagName==="B"?n.parentElement:n;const st=getComputedStyle(e);if(!e.disabled&&e.getClientRects().length&&st.visibility!=="hidden"&&st.display!=="none")return p}return ""})()`
	selected := ""
	for {
		view, err := b.loginView(ctx)
		if err != nil {
			return "", err
		}
		before = []byte(view.HTML)
		if c.Already(before) {
			return "already", nil
		}
		if c.LoggedIn(before) {
			if err = chromedp.Run(ctx, chromedp.Evaluate(expression, &selected)); err != nil {
				return "", ErrResponse
			}
			if selected != "" {
				break
			}
		}
		if !time.Now().Before(deadline) {
			if c.LoggedIn(before) {
				return "authenticated", nil
			}
			return "", ErrLogin
		}
		if err = loginDelay(ctx, 150*time.Millisecond); err != nil {
			return "", err
		}
	}
	if b.request.Guard(ctx) != nil {
		return "", ErrScope
	}
	finish, err := c.Action(ctx, "checkin-click")
	if err != nil || finish == nil {
		return "", ErrScope
	}
	path, _ := json.Marshal(selected)
	var clicked bool
	click := `(()=>{const n=document.evaluate(` + string(path) + `,document,null,XPathResult.FIRST_ORDERED_NODE_TYPE,null).singleNodeValue;if(!n)return false;const e=n.tagName==="B"?n.parentElement:n;if(e.disabled||!e.getClientRects().length)return false;e.click();return true})()`
	if err = chromedp.Run(ctx, chromedp.Evaluate(click, &clicked)); err != nil || !clicked {
		_ = finish("uncertain", "")
		return "", ErrResponse
	}
	if err = finish("received", Hash([]string{selected, "clicked"})); err != nil {
		return "", ErrResponse
	}
	deadline = time.Now().Add(12 * time.Second)
	for {
		view, err := b.loginView(ctx)
		if err == nil && c.Completed(before, []byte(view.HTML)) {
			return "signed", nil
		}
		if err == nil && c.Already([]byte(view.HTML)) {
			return "signed", nil
		}
		if err == ErrScope || err == ErrVerification || err == ErrLimit {
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "unknown", nil
		}
		if err = loginDelay(ctx, 150*time.Millisecond); err != nil {
			return "", err
		}
	}
}
