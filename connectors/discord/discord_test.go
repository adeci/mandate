package main

import (
	"strings"
	"testing"
)

func TestCheckToken(t *testing.T) {
	tests := []struct {
		name, content string
		ok            bool
	}{
		{"plain", "abc.def.ghi\n", true},
		{"empty", "  \n", false},
		{"bot token", "Bot abc.def.ghi", false},
		{"two tokens", "abc\ndef", false},
	}
	for _, tt := range tests {
		tok, err := checkToken(tt.content)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v", tt.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: error leaks the token: %v", tt.name, err)
		}
		if tt.ok && tok != "abc.def.ghi" {
			t.Errorf("%s: token %q", tt.name, tok)
		}
	}
}
