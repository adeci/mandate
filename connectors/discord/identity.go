package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

// The service presents itself as Google Chrome 156 on Linux, the owner's own
// browser; newBrowserClients explains the TLS profile.
const (
	chromeMajor  = 156
	systemLocale = "en-US"

	// defaultBuildNumber is the web client build observed on 2026-11-01,
	// used only until the first successful extraction.
	defaultBuildNumber = 657850
)

var (
	chromeVersion = strconv.Itoa(chromeMajor) + ".0.0.0"
	userAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
		chromeVersion + " Safari/537.36"
	secChUA = secCHUA(chromeMajor)
)

// secCHUA renders Chrome's Sec-Ch-Ua brand list for a major version, using
// Chromium's GREASE algorithm (components/embedder_support/user_agent_utils.cc)
// with the major version as the seed.
func secCHUA(major int) string {
	greaseChars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	greaseVersions := []string{"8", "99", "24"}
	orders := [6][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	v := strconv.Itoa(major)
	brands := [3]string{
		`"Not` + greaseChars[major%len(greaseChars)] + "A" + greaseChars[(major+1)%len(greaseChars)] +
			`Brand";v="` + greaseVersions[major%len(greaseVersions)] + `"`,
		`"Chromium";v="` + v + `"`,
		`"Google Chrome";v="` + v + `"`,
	}
	var out [3]string
	for i, pos := range orders[major%len(orders)] {
		out[pos] = brands[i]
	}
	return strings.Join(out[:], ", ")
}

// Field order follows the web client's construction of X-Super-Properties.
type superProperties struct {
	OS                       string  `json:"os"`
	Browser                  string  `json:"browser"`
	Device                   string  `json:"device"`
	DeviceModelName          string  `json:"device_model_name"`
	SystemLocale             string  `json:"system_locale"`
	HasClientMods            bool    `json:"has_client_mods"`
	BrowserUserAgent         string  `json:"browser_user_agent"`
	BrowserVersion           string  `json:"browser_version"`
	OSVersion                string  `json:"os_version"`
	Referrer                 string  `json:"referrer"`
	ReferringDomain          string  `json:"referring_domain"`
	ReferrerCurrent          string  `json:"referrer_current"`
	ReferringDomainCurrent   string  `json:"referring_domain_current"`
	ReleaseChannel           string  `json:"release_channel"`
	ClientBuildNumber        int     `json:"client_build_number"`
	ClientEventSource        *string `json:"client_event_source"`
	ClientLaunchID           string  `json:"client_launch_id"`
	LaunchSignature          string  `json:"launch_signature"`
	ClientHeartbeatSessionID string  `json:"client_heartbeat_session_id"`
	ClientAppState           string  `json:"client_app_state"`
}

func encodeSuperProperties(st *state) string {
	b, err := json.Marshal(superProperties{
		OS:                       "Linux",
		Browser:                  "Chrome",
		SystemLocale:             systemLocale,
		BrowserUserAgent:         userAgent,
		BrowserVersion:           chromeVersion,
		ReleaseChannel:           "stable",
		ClientBuildNumber:        st.buildNumber(),
		ClientLaunchID:           st.LaunchID,
		LaunchSignature:          st.LaunchSignature,
		ClientHeartbeatSessionID: st.HeartbeatSessionID,
		ClientAppState:           "focused",
	})
	if err != nil {
		panic(err) // fixed struct of strings, ints, and bools
	}
	return base64.StdEncoding.EncodeToString(b)
}

// identifyProperties are what the web client's fast-connect script sends in
// IDENTIFY, in its field order: the super-properties without the per-tab
// fields.
type identifyProperties struct {
	OS                     string  `json:"os"`
	Browser                string  `json:"browser"`
	Device                 string  `json:"device"`
	DeviceModelName        string  `json:"device_model_name"`
	SystemLocale           string  `json:"system_locale"`
	HasClientMods          bool    `json:"has_client_mods"`
	BrowserUserAgent       string  `json:"browser_user_agent"`
	BrowserVersion         string  `json:"browser_version"`
	OSVersion              string  `json:"os_version"`
	Referrer               string  `json:"referrer"`
	ReferringDomain        string  `json:"referring_domain"`
	ReferrerCurrent        string  `json:"referrer_current"`
	ReferringDomainCurrent string  `json:"referring_domain_current"`
	ReleaseChannel         string  `json:"release_channel"`
	ClientBuildNumber      int     `json:"client_build_number"`
	ClientEventSource      *string `json:"client_event_source"`
	ClientLaunchID         string  `json:"client_launch_id"`
	IsFastConnect          bool    `json:"is_fast_connect"`
	InstallationID         string  `json:"installation_id,omitempty"`
}

func newIdentifyProperties(buildNumber int, launchID, installationID string, fast bool) identifyProperties {
	return identifyProperties{
		OS:                "Linux",
		Browser:           "Chrome",
		SystemLocale:      systemLocale,
		BrowserUserAgent:  userAgent,
		BrowserVersion:    chromeVersion,
		ReleaseChannel:    "stable",
		ClientBuildNumber: buildNumber,
		ClientLaunchID:    launchID,
		IsFastConnect:     fast,
		InstallationID:    installationID,
	}
}

// Slices of header keep Chrome's wire order.
type header struct{ name, value string }

// call is what Discord's code adds to one API request.
type call struct {
	token    string // "" while logged out
	timezone string
	referer  string
	post     bool // Chrome adds Origin
	json     bool // a JSON body
	// context is the location in X-Context-Properties, "" for none.
	context     string
	fingerprint string // what /experiments assigned a logged-out browser
	// superFirst: the caller passes X-Super-Properties itself, so it comes
	// ahead of the headers Discord's request layer sets.
	superFirst bool
}

// Discord's own headers go out in the order its code sets them, between
// the ones Chrome adds.
func apiHeaders(st *state, c call) []header {
	hs := make([]header, 0, 24)
	hs = append(hs, header{"sec-ch-ua-platform", `"Linux"`})
	if c.json {
		hs = append(hs, header{"content-type", "application/json"})
	}
	sp := header{"x-super-properties", encodeSuperProperties(st)}
	if c.superFirst {
		hs = append(hs, sp)
	}
	if c.context != "" {
		hs = append(hs, header{"x-context-properties", contextProperties(c.context)})
	}
	if c.token != "" {
		hs = append(hs, header{"authorization", c.token})
	}
	if !c.superFirst {
		hs = append(hs, sp)
	}
	if c.fingerprint != "" {
		hs = append(hs, header{"x-fingerprint", c.fingerprint})
	}
	if st.InstallationID != "" {
		hs = append(hs, header{"x-installation-id", st.InstallationID})
	}
	hs = append(hs,
		header{"x-discord-locale", systemLocale},
		header{"x-discord-timezone", c.timezone},
		header{"x-debug-options", "bugReporterEnabled"},
		header{"sec-ch-ua", secChUA},
		header{"sec-ch-ua-mobile", "?0"},
		header{"user-agent", userAgent},
		header{"accept", "*/*"},
	)
	if c.post {
		hs = append(hs, header{"origin", defaultWebBase})
	}
	return append(hs,
		header{"sec-fetch-site", "same-origin"},
		header{"sec-fetch-mode", "cors"},
		header{"sec-fetch-dest", "empty"},
		header{"referer", c.referer},
		header{"accept-encoding", "gzip, deflate, br, zstd"},
		header{"accept-language", "en-US,en;q=0.9"},
		header{"priority", "u=1, i"},
	)
}

// contextProperties encodes {"location": location} as the web client does.
func contextProperties(location string) string {
	b, err := json.Marshal(struct {
		Location string `json:"location"`
	}{location})
	if err != nil {
		panic(err) // a string
	}
	return base64.StdEncoding.EncodeToString(b)
}

// No referrer and no token: a navigation typed into the address bar.
func documentHeaders() []header {
	return []header{
		{"sec-ch-ua", secChUA},
		{"sec-ch-ua-mobile", "?0"},
		{"sec-ch-ua-platform", `"Linux"`},
		{"upgrade-insecure-requests", "1"},
		{"user-agent", userAgent},
		{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		{"sec-fetch-site", "none"},
		{"sec-fetch-mode", "navigate"},
		{"sec-fetch-user", "?1"},
		{"sec-fetch-dest", "document"},
		{"accept-encoding", "gzip, deflate, br, zstd"},
		{"accept-language", "en-US,en;q=0.9"},
		{"priority", "u=0, i"},
	}
}

// A file opened from the app in a new tab: a navigation to the CDN, which
// gets only discord.com's origin as referrer.
func attachmentHeaders() []header {
	return []header{
		{"sec-ch-ua", secChUA},
		{"sec-ch-ua-mobile", "?0"},
		{"sec-ch-ua-platform", `"Linux"`},
		{"upgrade-insecure-requests", "1"},
		{"user-agent", userAgent},
		{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		{"sec-fetch-site", "cross-site"},
		{"sec-fetch-mode", "navigate"},
		{"sec-fetch-user", "?1"},
		{"sec-fetch-dest", "document"},
		{"referer", defaultWebBase + "/"},
		{"accept-encoding", "gzip, deflate, br, zstd"},
		{"accept-language", "en-US,en;q=0.9"},
		{"priority", "u=0, i"},
	}
}

func scriptHeaders(referer string) []header {
	return []header{
		{"sec-ch-ua-platform", `"Linux"`},
		{"user-agent", userAgent},
		{"sec-ch-ua", secChUA},
		{"sec-ch-ua-mobile", "?0"},
		{"accept", "*/*"},
		{"sec-fetch-site", "same-origin"},
		{"sec-fetch-mode", "no-cors"},
		{"sec-fetch-dest", "script"},
		{"referer", referer},
		{"accept-encoding", "gzip, deflate, br, zstd"},
		{"accept-language", "en-US,en;q=0.9"},
		{"priority", "u=1"},
	}
}

// Cookies from the jar go out after accept-language and a POST's length
// first, where Chrome sends them.
func applyHeaders(req *fhttp.Request, hs []header) {
	order := make([]string, 0, len(hs)+2)
	if req.ContentLength > 0 || req.Method == fhttp.MethodPost {
		order = append(order, "content-length")
	}
	for _, h := range hs {
		if h.name == "priority" {
			order = append(order, "cookie")
		}
		req.Header.Set(h.name, h.value)
		order = append(order, h.name)
	}
	req.Header[fhttp.HeaderOrderKey] = order
}

// gatewayHeaders are Chrome's WebSocket handshake headers for the app's
// gateway connection, which carries no cookies and no client hints. The
// websocket package adds Connection, Upgrade and the Sec-WebSocket ones.
// Chrome also offers permessage-deflate with context takeover, which the
// package cannot decode, so no extension is offered.
func gatewayHeaders() fhttp.Header {
	return fhttp.Header{
		"Pragma":          {"no-cache"},
		"Cache-Control":   {"no-cache"},
		"User-Agent":      {userAgent},
		"Origin":          {defaultWebBase},
		"Accept-Encoding": {"gzip, deflate, br, zstd"},
		"Accept-Language": {"en-US,en;q=0.9"},
		fhttp.HeaderOrderKey: {
			"host", "connection", "pragma", "cache-control", "user-agent", "upgrade", "origin",
			"sec-websocket-version", "accept-encoding", "accept-language", "sec-websocket-key",
		},
	}
}

var (
	webJSRe         = regexp.MustCompile(`<script[^>]*\ssrc="(/assets/web\.[a-f0-9]{8,32}\.js)"`)
	buildNumberJSRe = regexp.MustCompile(`(?:buildNumber|build_number):\s?['"]?(\d{6,9})['"]?`)
	// Fallback: the page's GLOBAL_ENV.
	buildNumberHTMLRe = regexp.MustCompile(`"BUILD_NUMBER"\s*:\s*"(\d{6,9})"`)
	// Installation IDs and fingerprints are "<snowflake>.<base64url>"; only
	// that shape is ever put into a header.
	issuedIDRe = regexp.MustCompile(`^\d{15,21}\.[A-Za-z0-9_-]{20,64}$`)
)

// The path is always /assets/web.<hex>.js, so it is safe to request.
func webJSPath(html []byte) (string, bool) {
	m := webJSRe.FindSubmatch(html)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

func extractBuildNumber(re *regexp.Regexp, b []byte) (int, bool) {
	m := re.FindSubmatch(b)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(m[1]))
	return n, err == nil
}

func newUUID() string { return formatUUID(uuidV4()) }

func uuidV4() [16]byte {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return b
}

// launchSignatureBits are the client-mod detection bits of a launch
// signature (https://docs.discord.food/reference#launch-signature).
var launchSignatureBits = []int{119, 108, 100, 91, 84, 75, 61, 55, 48, 38, 24, 11}

// An unmodified browser leaves every client-mod bit clear.
func newLaunchSignature() string {
	b := uuidV4()
	for _, bit := range launchSignatureBits {
		b[(127-bit)/8] &^= 1 << (bit % 8)
	}
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
