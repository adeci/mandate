package authfilter

import "testing"

func TestIsAuth(t *testing.T) {
	c, err := New([]string{`(?i)^bank-alerts$`}, []string{`(?i)recovery phrase`})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, sender, body string
		want               bool
	}{
		{"google prefixed code", "x", "G-482913 is your Google verification code.", true},
		{"dashed whatsapp code", "x", "Your WhatsApp code: 482-913", true},
		{"spaced code", "x", "Your login code is 4 8 2 9 1 3", true},
		{"space-grouped code", "15550100@s.whatsapp.net", "Your security code: 482 913. Expires in 10 minutes.", true},
		{"apple code", "x", "Your Apple Account code is: 482913. Don't share it with anyone.", true},
		{"pin", "x", "Your PIN is 4829", true},

		{"magic link", "x", "Tap to sign in: https://example.com/auth/magic?t=abc", true},
		{"verify link no keyword", "x", "https://example.com/verify-email?id=9", true},

		{"extra sender pattern", "BANK-ALERTS", "Purchase at store", true},
		{"extra body pattern", "x", "here is my recovery phrase lol", true},

		{"plain chat", "x", "see you at 5pm!", false},
		{"number without auth context", "x", "your package 482913 was delivered", false},
		{"auth word without code", "x", "did you finish the code review?", false},
		{"phone number in chat", "x", "call me at 555-1234 tomorrow", false},
		{"ordinary link", "x", "look https://example.com/blog/rust-tips", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.IsAuth(tt.sender, tt.body); got != tt.want {
				t.Fatalf("IsAuth(%q, %q) = %v, want %v", tt.sender, tt.body, got, tt.want)
			}
		})
	}
}
