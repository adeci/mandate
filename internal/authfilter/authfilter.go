// Package authfilter flags messages that carry login material: one-time codes
// and magic/reset links. It is deliberately biased toward false positives:
// hiding a harmless message costs little, serving a live code does not.
package authfilter

import (
	"fmt"
	"regexp"
)

var (
	keyword = regexp.MustCompile(`(?i)\b(?:code|otp|passcode|pass code|password|pin|verif\w*|2fa|two[- ]factor|one[- ]time|login|log in|sign[- ]?in|security|authenticat\w*|confirm\w*|do not share|don'?t share|never share|expires?|valid for)\b`)

	// Code-like tokens: 4-8 digits, digit groups split by space/dash/dot
	// (123 456, 123-456, 1 2 3 4 5 6), or prefixed alphanumerics (G-123456).
	codeToken = regexp.MustCompile(`(?i)(?:\b\d{4,8}\b|\b\d{2,4}[ .\-]\d{2,4}\b|\b(?:\d ){3,7}\d\b|\b[a-z]{1,3}-\d{4,8}\b)`)

	magicLink = regexp.MustCompile(`(?i)https?://\S*(?:token|verif|auth|reset|login|signin|sign-in|magic|otp|confirm|activate|passwordless)\S*`)
)

type Classifier struct {
	extraSender []*regexp.Regexp
	extraBody   []*regexp.Regexp
}

func New(extraSender, extraBody []string) (*Classifier, error) {
	c := &Classifier{}
	for _, p := range extraSender {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("auth sender pattern %q: %w", p, err)
		}
		c.extraSender = append(c.extraSender, re)
	}
	for _, p := range extraBody {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("auth body pattern %q: %w", p, err)
		}
		c.extraBody = append(c.extraBody, re)
	}
	return c, nil
}

func (c *Classifier) IsAuth(sender, body string) bool {
	for _, re := range c.extraSender {
		if re.MatchString(sender) {
			return true
		}
	}
	for _, re := range c.extraBody {
		if re.MatchString(body) {
			return true
		}
	}
	if magicLink.MatchString(body) {
		return true
	}
	return keyword.MatchString(body) && codeToken.MatchString(body)
}
