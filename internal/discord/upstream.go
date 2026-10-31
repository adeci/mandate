package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"github.com/adeci/mandate/internal/exitcode"
)

const (
	defaultAPIBase = "https://discord.com/api/v9"
	defaultWebBase = "https://discord.com"

	appPath          = "/channels/@me"
	requestTimeout   = 15 * time.Second
	maxAPIBody       = 8 << 20
	maxAssetBody     = 32 << 20
	heartbeatTimeout = 30 * time.Minute
	buildRefresh     = 24 * time.Hour
	denialTTL        = 24 * time.Hour
)

type transport interface {
	Do(req *fhttp.Request) (*fhttp.Response, error)
}

// ownerCodes are JSON error codes that mean the account needs manual action
// (https://docs.discord.food/topics/errors and the datamined code list).
var ownerCodes = map[int]string{
	20013: "account disabled",
	40001: "unauthorized",
	40002: "account verification required",
	40068: "account quarantined",
	40333: "request blocked by Cloudflare",
	50178: "account verification required",
	60004: "verified account required",
	70007: "phone verification required",
}

type pacedError struct {
	retryAfter time.Duration
	reason     string
}

func (e *pacedError) Error() string {
	return fmt.Sprintf("discord pacing (%s): retry in %ds", e.reason, retrySeconds(e.retryAfter))
}

type deniedError struct {
	status int
	what   string
	until  time.Time
}

func (e *deniedError) Error() string {
	if e.until.IsZero() {
		return e.what
	}
	return fmt.Sprintf("discord answered %d for %s (not retried until %s)", e.status, e.what, e.until.UTC().Format(time.RFC3339))
}

type notReadyError struct{ retryAfter time.Duration }

func (e *notReadyError) Error() string { return "discord search index not ready" }

type upstreamError struct {
	status int // 0 for transport failures
	msg    string
}

func (e *upstreamError) Error() string {
	if e.status == 0 {
		return "discord request failed: " + e.msg
	}
	return fmt.Sprintf("discord answered %d%s", e.status, e.msg)
}

func retrySeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

func ownerError(reason string) error {
	return fmt.Errorf("discord: %s: %w", reason, exitcode.ErrNeedsOwner)
}

// route is built only from validated IDs.
type route struct {
	template string     // path template for logs, e.g. /channels/{id}/messages
	path     string     // concrete path below the API base
	query    url.Values // nil for none
	major    string     // top-level resource ID, for rate-limit buckets
	referer  string     // page the request appears to come from
	deny     string     // denial-cache key ("channel:<id>"), "" if not cacheable
	denyWhat string     // human name for the denial
}

func (r route) key() string { return r.template + "|" + r.major }

// Only the worker goroutine uses upstream.
type upstream struct {
	tr        transport
	jar       fhttp.CookieJar // nil in tests
	token     string
	timezone  string
	apiBase   string
	webBase   string
	clock     clock
	pace      *pacer
	st        *state
	statePath string
	log       *slog.Logger
	fatal     error // set once the account needs manual action; every later call fails
}

func (u *upstream) denied(key, what string) error {
	if key == "" {
		return nil
	}
	if d, ok := u.st.Denied[key]; ok && d.Until.After(u.clock.Now()) {
		return &deniedError{status: d.Status, what: what, until: d.Until}
	}
	return nil
}

// dl is the latest acceptable slot for any request of the current call.
func (u *upstream) get(ctx context.Context, dl time.Time, r route, out any) error {
	if u.fatal != nil {
		return u.fatal
	}
	if err := u.denied(r.deny, r.denyWhat); err != nil {
		return err
	}
	if err := u.ensureBuildNumber(ctx, dl); err != nil {
		return err
	}
	var notBefore time.Time
	for attempt := 0; ; attempt++ {
		resp, body, err := u.send(ctx, dl, kindAPI, r, notBefore)
		if err != nil {
			return err
		}
		now := u.clock.Now()
		if reason := ownerProblem(resp.StatusCode, body); reason != "" {
			u.fatal = ownerError(reason)
			u.log.Error("discord account needs manual action; stopping", "reason", reason, "status", resp.StatusCode, "route", r.template)
			return u.fatal
		}
		switch s := resp.StatusCode; {
		case s == fhttp.StatusAccepted:
			wait := retryAfter(resp, body)
			if wait <= 0 {
				wait = notReadyRetryAfter
			}
			if attempt > 0 {
				return &notReadyError{retryAfter: wait}
			}
			notBefore = now.Add(wait)
		case s >= 200 && s < 300:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return &upstreamError{status: s, msg: ": undecodable body"}
			}
			return nil
		case s == fhttp.StatusTooManyRequests:
			wait := retryAfter(resp, body)
			if err := u.rateLimited(now, resp, body, wait); err != nil {
				return err
			}
			if attempt > 0 {
				return &pacedError{retryAfter: wait, reason: "rate limited by discord"}
			}
			notBefore = now.Add(wait)
		case (s == fhttp.StatusForbidden || s == fhttp.StatusNotFound) && r.deny != "":
			d := denial{Status: s, Until: now.Add(denialTTL)}
			if u.st.Denied == nil {
				u.st.Denied = make(map[string]denial)
			}
			u.st.Denied[r.deny] = d
			if err := u.saveState(); err != nil {
				return err
			}
			return &deniedError{status: s, what: r.denyWhat, until: d.Until}
		default:
			return &upstreamError{status: s, msg: errorCode(body)}
		}
	}
}

// rateLimited records a 429 and enters the cooldown when it is global, asks
// for a long wait (or none at all), or is one too many this hour.
func (u *upstream) rateLimited(now time.Time, resp *fhttp.Response, body []byte, wait time.Duration) error {
	st := u.st
	st.prune(now)
	st.RateLimits = append(st.RateLimits, now)
	var b struct {
		Global bool `json:"global"`
	}
	_ = json.Unmarshal(body, &b)
	global := b.Global || resp.Header.Get("X-RateLimit-Global") == "true" || resp.Header.Get("X-RateLimit-Scope") == "global"
	var why string
	switch {
	case global:
		why = "global rate limit"
	case wait <= 0:
		why = "rate limit without retry_after"
	case wait > rateLimitMaxObey:
		why = "long retry_after"
	case len(st.RateLimits) >= rateLimitTrips:
		why = "repeated rate limits"
	}
	if why == "" {
		return u.saveState()
	}
	st.CooldownUntil = now.Add(cooldownLength)
	u.log.Warn("discord rate limit; cooling down", "why", why, "retry_after", wait, "until", st.CooldownUntil.Format(time.RFC3339))
	if err := u.saveState(); err != nil {
		return err
	}
	return &pacedError{retryAfter: cooldownLength, reason: "cooldown after " + why}
}

func (u *upstream) send(ctx context.Context, dl time.Time, kind requestKind, r route, notBefore time.Time) (*fhttp.Response, []byte, error) {
	now := u.clock.Now()
	at, reason := u.pace.slot(now, kind, r.key(), notBefore)
	if at.After(dl) {
		return nil, nil, &pacedError{retryAfter: at.Sub(now), reason: reason}
	}
	wait := at.Sub(now)
	if err := u.clock.Sleep(ctx, wait); err != nil {
		return nil, nil, err
	}

	var target string
	var hs []header
	switch {
	case kind == kindSubresource:
		target = u.webBase + r.path
		hs = scriptHeaders(r.referer)
	case r.template == appPath:
		target = u.webBase + r.path
		hs = documentHeaders()
	default:
		target = u.apiBase + r.path
		u.touchHeartbeat(u.clock.Now())
		hs = apiHeaders(u.st, u.token, u.timezone, r.referer)
	}
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := fhttp.NewRequestWithContext(rctx, fhttp.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	applyHeaders(req, hs)

	sent := u.clock.Now()
	resp, err := u.tr.Do(req)
	u.pace.record(sent)
	limit := int64(maxAPIBody)
	if kind == kindSubresource || r.template == appPath {
		limit = maxAssetBody
	}
	var body []byte
	if err == nil {
		body, err = io.ReadAll(io.LimitReader(resp.Body, limit))
		resp.Body.Close()
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
		u.pace.updateBucket(u.clock.Now(), r.key(), resp.Header.Get("X-RateLimit-Bucket"),
			resp.Header.Get("X-RateLimit-Remaining"), resp.Header.Get("X-RateLimit-Reset-After"))
		u.keepCookies(sent, resp)
	}
	u.log.Info("upstream", "method", fhttp.MethodGet, "route", r.template, "status", status,
		"wait", wait.Round(time.Millisecond), "wait_reason", reason,
		"duration", u.clock.Now().Sub(sent).Round(time.Millisecond),
		"budget_used", len(u.st.Requests))
	if serr := u.saveState(); serr != nil {
		return nil, nil, serr
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// Drop the URL from the error: a search URL carries the query.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, nil, &upstreamError{msg: r.template + ": " + err.Error()}
	}
	return resp, body, nil
}

// touchHeartbeat rotates the analytics heartbeat session after 30 minutes
// without use, as the web client does.
func (u *upstream) touchHeartbeat(now time.Time) {
	if u.st.HeartbeatSessionID == "" || now.Sub(u.st.HeartbeatUsedAt) >= heartbeatTimeout {
		u.st.HeartbeatSessionID = newUUID()
	}
	u.st.HeartbeatUsedAt = now
}

// ensureBuildNumber loads the app page and its main bundle at most once per
// 24 h, like opening Discord in the browser, to learn the client build
// number. Failures keep the last known number.
func (u *upstream) ensureBuildNumber(ctx context.Context, dl time.Time) error {
	if u.clock.Now().Sub(u.st.BuildCheckedAt) < buildRefresh {
		return nil
	}
	page := route{template: appPath, path: appPath}
	resp, html, err := u.send(ctx, dl, kindAPI, page, time.Time{})
	var pe *pacedError
	if errors.As(err, &pe) || ctx.Err() != nil {
		return err // nothing was sent
	}
	if u.fatal != nil {
		return u.fatal
	}
	u.st.BuildCheckedAt = u.clock.Now()
	if err != nil || resp.StatusCode != fhttp.StatusOK {
		u.log.Warn("loading the app page failed; keeping the last build number", "status", statusOf(resp), "err", err, "build_number", u.st.buildNumber())
		return u.saveState()
	}
	found, ok := extractBuildNumber(buildNumberHTMLRe, html)
	if js, jsOK := webJSPath(html); jsOK {
		script := route{template: "/assets/web.{hash}.js", path: js, referer: u.webBase + appPath}
		resp, body, err := u.send(ctx, dl, kindSubresource, script, time.Time{})
		if errors.As(err, &pe) || ctx.Err() != nil {
			if ok {
				u.st.BuildNumber = found
			}
			if serr := u.saveState(); serr != nil {
				return serr
			}
			return err
		}
		if err == nil && resp.StatusCode == fhttp.StatusOK {
			if n, nOK := extractBuildNumber(buildNumberJSRe, body); nOK {
				found, ok = n, true
			}
		}
	}
	if !ok {
		u.log.Warn("no build number in the app page or bundle; keeping the last one", "build_number", u.st.buildNumber())
		return u.saveState()
	}
	if found != u.st.BuildNumber {
		u.log.Info("client build number", "build_number", found, "previous", u.st.BuildNumber)
	}
	u.st.BuildNumber = found
	return u.saveState()
}

// keepCookies persists discord.com's persistent cookies so the next process
// continues the same browser profile.
func (u *upstream) keepCookies(now time.Time, resp *fhttp.Response) {
	for _, c := range resp.Cookies() {
		exp := c.Expires
		if c.MaxAge > 0 {
			exp = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		kept := u.st.Cookies[:0]
		for _, old := range u.st.Cookies {
			if old.Name != c.Name || old.Domain != c.Domain || old.Path != c.Path {
				kept = append(kept, old)
			}
		}
		u.st.Cookies = kept
		if c.MaxAge < 0 || exp.IsZero() || !exp.After(now) {
			continue // deleted, or a session cookie
		}
		u.st.Cookies = append(u.st.Cookies, cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain,
			Path: c.Path, Expires: exp, Secure: c.Secure, HTTPOnly: c.HttpOnly,
		})
	}
}

func (u *upstream) restoreCookies() error {
	if u.jar == nil || len(u.st.Cookies) == 0 {
		return nil
	}
	base, err := url.Parse(u.webBase)
	if err != nil {
		return err
	}
	cs := make([]*fhttp.Cookie, 0, len(u.st.Cookies))
	for _, c := range u.st.Cookies {
		cs = append(cs, &fhttp.Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: c.Expires, Secure: c.Secure, HttpOnly: c.HTTPOnly,
		})
	}
	u.jar.SetCookies(base, cs)
	return nil
}

func (u *upstream) saveState() error {
	if err := u.st.save(u.statePath); err != nil {
		u.fatal = err
		u.log.Error("cannot persist pacing state; stopping", "err", err)
		return err
	}
	return nil
}

func statusOf(resp *fhttp.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func ownerProblem(status int, body []byte) string {
	if status == fhttp.StatusUnauthorized {
		return "token rejected (401)"
	}
	var e struct {
		Code           int             `json:"code"`
		CaptchaKey     json.RawMessage `json:"captcha_key"`
		CaptchaSitekey json.RawMessage `json:"captcha_sitekey"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "" // not an object: a list or non-JSON page
	}
	switch {
	case len(e.CaptchaKey) > 0 || len(e.CaptchaSitekey) > 0:
		return "captcha required"
	case ownerCodes[e.Code] != "":
		return fmt.Sprintf("%s (code %d)", ownerCodes[e.Code], e.Code)
	case status == fhttp.StatusForbidden && e.Code == 10008:
		return "request blocked by anti-abuse (403, code 10008)"
	}
	return ""
}

func retryAfter(resp *fhttp.Response, body []byte) time.Duration {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &b) == nil && b.RetryAfter > 0 {
		return time.Duration(b.RetryAfter * float64(time.Second))
	}
	if s, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return 0
}

// errorCode renders a Discord JSON error code (never its message, which may
// quote user content) for logs and errors.
func errorCode(body []byte) string {
	var e struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil && e.Code != 0 {
		return fmt.Sprintf(" (code %d)", e.Code)
	}
	return ""
}
