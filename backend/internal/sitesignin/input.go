package sitesignin

import (
	"net/mail"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

// ChallengeLimits describes characters, while the original provider's byte
// limits remain enforced below. Saved-account captcha lengths are variable;
// HDSky/OpenCD explicitly set six characters when creating their challenge.
func ChallengeLimits(c Challenge) (int, int) {
	min, max := c.MinLength, c.MaxLength
	if min <= 0 {
		min = 1
	}
	if max <= 0 {
		max = 128
		if c.Kind == "email" {
			max = 254
		}
	}
	return min, max
}
func ValidateInput(c Challenge, values []string) error {
	if len(c.Choices) > 0 {
		if c.Kind != "choice" || !validChoices(values, c.Choices) || !c.Multiple && len(values) != 1 {
			return ErrInput
		}
		return nil
	}
	if len(values) != 1 {
		return ErrInput
	}
	value := values[0]
	limit := 128
	switch c.Kind {
	case "captcha", "two_step", "email_code":
	case "email":
		limit = 254
	default:
		return ErrInput
	}
	min, max := ChallengeLimits(c)
	length := utf8.RuneCountInString(value)
	if min < 1 || max < min || max > 4096 || !sitelogin.Text(value, limit) || length < min || length > max {
		return ErrInput
	}
	if c.Kind == "email" {
		address, e := mail.ParseAddress(value)
		if e != nil || address.Address != value {
			return ErrInput
		}
	}
	return nil
}
