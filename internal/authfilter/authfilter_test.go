package authfilter

import "testing"

func TestIsAuth(t *testing.T) {
	c, err := New([]string{`(?i)^bank-alerts$`}, []string{`(?i)recovery phrase`})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, source, sender, body string
		want                       bool
	}{
		{"google prefixed code", "sms", "+15550100", "G-482913 is your Google verification code.", true},
		{"bank do-not-share", "sms", "+15550100", "Bank: DON'T share. Use code 48291377 to verify your identity.", true},
		{"dashed whatsapp code", "sms", "+15550100", "Your WhatsApp code: 482-913", true},
		{"spaced code", "sms", "+15550100", "Your login code is 4 8 2 9 1 3", true},
		{"space-grouped code", "whatsapp", "15550100@s.whatsapp.net", "Your security code: 482 913. Expires in 10 minutes.", true},
		{"discord security code", "discord", "1234", "Your Discord security code is 482913", true},
		{"apple code", "sms", "+15550100", "Your Apple Account code is: 482913. Don't share it with anyone.", true},
		{"pin", "sms", "+15550100", "Your PIN is 4829", true},

		{"magic link", "sms", "+15550100", "Tap to sign in: https://example.com/auth/magic?t=abc", true},
		{"reset link", "whatsapp", "x", "Reset here https://example.com/password-reset/abc123", true},
		{"verify link no keyword", "discord", "x", "https://example.com/verify-email?id=9", true},

		{"sms short code without code text", "sms", "72975", "Your balance is low.", true},
		{"short code only applies to sms", "whatsapp", "72975", "hello there", false},
		{"extra sender pattern", "sms", "BANK-ALERTS", "Purchase at store", true},
		{"extra body pattern", "discord", "x", "here is my recovery phrase lol", true},

		{"plain chat", "sms", "+15550100", "see you at 5pm!", false},
		{"number without auth context", "sms", "+15550100", "your package 482913 was delivered", false},
		{"auth word without code", "whatsapp", "x", "did you finish the code review?", false},
		{"phone number in chat", "sms", "+15550100", "call me at 555-1234 tomorrow", false},
		{"ordinary link", "discord", "x", "look https://example.com/blog/rust-tips", false},
		{"empty", "sms", "+15550100", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.IsAuth(tt.source, tt.sender, tt.body); got != tt.want {
				t.Fatalf("IsAuth(%q, %q, %q) = %v, want %v", tt.source, tt.sender, tt.body, got, tt.want)
			}
		})
	}
}

func TestNewRejectsBadPattern(t *testing.T) {
	if _, err := New(nil, []string{"("}); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}
