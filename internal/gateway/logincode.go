package gateway

import "regexp"

// Login material: one-time codes and magic or reset links. The patterns err
// toward hiding: a harmless message hidden costs little, a live code served
// does not.
var (
	codeWord = regexp.MustCompile(`(?i)\b(?:code|otp|passcode|pass code|password|pin|verif\w*|2fa|two[- ]factor|one[- ]time|login|log in|sign[- ]?in|security|authenticat\w*|confirm\w*|do not share|don'?t share|never share|expires?|valid for)\b`)

	// 4-8 digits, digit groups split by space, dash or dot (123 456,
	// 1 2 3 4 5 6), or prefixed (G-123456).
	codeToken = regexp.MustCompile(`(?i)(?:\b\d{4,8}\b|\b\d{2,4}[ .\-]\d{2,4}\b|\b(?:\d ){3,7}\d\b|\b[a-z]{1,3}-\d{4,8}\b)`)

	magicLink = regexp.MustCompile(`(?i)https?://\S*(?:token|verif|auth|reset|login|signin|sign-in|magic|otp|confirm|activate|passwordless)\S*`)
)

func loginCode(text string) bool {
	return magicLink.MatchString(text) || codeWord.MatchString(text) && codeToken.MatchString(text)
}
