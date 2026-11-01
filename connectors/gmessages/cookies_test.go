package main

import (
	"maps"
	"strings"
	"testing"
)

const jar = "SID=sid1; HSID=hsid1; SSID=ssid1; APISID=api/1; SAPISID=sapi/1; OSID=osid=1; __Secure-1PSIDTS=ts1; NID=other"

func TestParseCookies(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"SID": "sid1", "HSID": "hsid1", "SSID": "ssid1", "APISID": "api/1", "SAPISID": "sapi/1", "OSID": "osid=1",
		"__Secure-1PSIDTS": "ts1",
	}
	for name, in := range map[string]string{
		"header":      jar,
		"named":       "  Cookie: " + jar + "\n",
		"chrome cURL": `curl 'https://messages.google.com/web/config' \  -H 'accept: */*' \  -b '` + jar + `' \  -H 'user-agent: Mozilla/5.0 (X11; Linux x86_64)'`,
		"firefox cURL": `curl 'https://messages.google.com/web/conversations' -H 'User-Agent: Mozilla/5.0' -H 'Cookie: ` + jar +
			`' -H 'Sec-Fetch-Site: same-origin'`,
		"ANSI-C quoted": `curl 'https://messages.google.com/web/' -b $'` + jar + `'`,
		"double quoted": `curl "https://messages.google.com/web/" --header "cookie: ` + jar + `"`,
		"JSON": `{"SID": "sid1", "HSID": "hsid1", "SSID": "ssid1", "APISID": "api/1", "SAPISID": "sapi/1", "OSID": "osid=1",
			"__Secure-1PSIDTS": "ts1", "NID": "other"}`,
	} {
		got, err := parseCookies(in)
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("%s: %v, %v", name, got, err)
		}
	}

	for name, tc := range map[string]struct{ in, says string }{
		"missing OSID":     {strings.Replace(jar, "OSID=osid=1; ", "", 1), "missing cookies OSID"},
		"another site":     {"session=abc; theme=dark", "missing cookies SID, HSID, SSID, APISID, SAPISID, OSID"},
		"cURL without any": {`curl 'https://messages.google.com/web/' -H 'accept: */*'`, "missing cookies SID"},
		"open quote":       {`curl 'https://messages.google.com/web/' -b 'SID=1`, "unclosed quote"},
		"JSON numbers":     {`{"SID": 1}`, "not an object of names to text values"},
	} {
		_, err := parseCookies(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want an error saying %q", name, err, tc.says)
		}
	}
}
