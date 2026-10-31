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
		{"google prefixed code", "whatsapp", "x", "G-482913 is your Google verification code.", true},
		{"bank do-not-share", "whatsapp", "x", "Bank: DON'T share. Use code 48291377 to verify your identity.", true},
		{"dashed whatsapp code", "whatsapp", "x", "Your WhatsApp code: 482-913", true},
		{"spaced code", "whatsapp", "x", "Your login code is 4 8 2 9 1 3", true},
		{"space-grouped code", "whatsapp", "15550100@s.whatsapp.net", "Your security code: 482 913. Expires in 10 minutes.", true},
		{"discord security code", "discord", "1234", "Your Discord security code is 482913", true},
		{"apple code", "whatsapp", "x", "Your Apple Account code is: 482913. Don't share it with anyone.", true},
		{"pin", "whatsapp", "x", "Your PIN is 4829", true},

		{"magic link", "whatsapp", "x", "Tap to sign in: https://example.com/auth/magic?t=abc", true},
		{"reset link", "whatsapp", "x", "Reset here https://example.com/password-reset/abc123", true},
		{"verify link no keyword", "discord", "x", "https://example.com/verify-email?id=9", true},

		{"extra sender pattern", "whatsapp", "BANK-ALERTS", "Purchase at store", true},
		{"extra body pattern", "discord", "x", "here is my recovery phrase lol", true},

		{"plain chat", "whatsapp", "x", "see you at 5pm!", false},
		{"number without auth context", "whatsapp", "x", "your package 482913 was delivered", false},
		{"auth word without code", "whatsapp", "x", "did you finish the code review?", false},
		{"phone number in chat", "whatsapp", "x", "call me at 555-1234 tomorrow", false},
		{"ordinary link", "discord", "x", "look https://example.com/blog/rust-tips", false},
		{"empty", "whatsapp", "x", "", false},
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
