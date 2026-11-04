package sitesignin

import (
	"strings"
	"testing"
)

func TestChallengeInputRetainsOriginalPerProviderLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      Challenge
		values []string
		valid  bool
	}{
		{"site six chars", Challenge{Kind: "captcha", MinLength: 6, MaxLength: 6}, []string{"ABC123"}, true},
		{"short does not consume", Challenge{Kind: "captcha", MinLength: 6, MaxLength: 6}, []string{"ABC"}, false},
		{"unicode characters", Challenge{Kind: "captcha", MinLength: 6, MaxLength: 6}, []string{"甲乙丙丁戊己"}, true},
		{"login captcha variable", Challenge{Kind: "captcha"}, []string{"ABCD"}, true},
		{"two step original variable", Challenge{Kind: "two_step"}, []string{"fixture-otp"}, true},
		{"email", Challenge{Kind: "email"}, []string{"fixture@example.test"}, true},
		{"display email rejected", Challenge{Kind: "email"}, []string{"Fixture <fixture@example.test>"}, false},
		{"invalid email", Challenge{Kind: "email"}, []string{"not-an-email"}, false},
		{"email code", Challenge{Kind: "email_code"}, []string{"123456"}, true},
		{"long code", Challenge{Kind: "email_code"}, []string{strings.Repeat("x", 129)}, false},
		{"unknown input type", Challenge{Kind: "password"}, []string{"fixture"}, false},
		{"single choice", Challenge{Kind: "choice", Choices: []Choice{{Value: "2"}, {Value: "4"}}}, []string{"2"}, true},
		{"single choice count", Challenge{Kind: "choice", Choices: []Choice{{Value: "2"}, {Value: "4"}}}, []string{"2", "4"}, false},
		{"multiple choices", Challenge{Kind: "choice", Multiple: true, Choices: []Choice{{Value: "2"}, {Value: "4"}}}, []string{"2", "4"}, true},
		{"duplicate choice", Challenge{Kind: "choice", Multiple: true, Choices: []Choice{{Value: "2"}, {Value: "4"}}}, []string{"2", "2"}, false},
		{"foreign choice", Challenge{Kind: "choice", Choices: []Choice{{Value: "2"}, {Value: "4"}}}, []string{"8"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if valid := ValidateInput(tc.c, tc.values) == nil; valid != tc.valid {
				t.Fatalf("accepted=%v expected=%v", valid, tc.valid)
			}
		})
	}
}
