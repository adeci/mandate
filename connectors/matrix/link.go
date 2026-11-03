package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/crypto/utils"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/adeci/mandate/connector"
)

const deviceName = "mandate"

const (
	setupTitle = "Link Matrix"
	// Verification steps change as the other device acts.
	pollSeconds = 2
)

var loginStep = connector.Step{
	ID: "login", Title: setupTitle,
	Text: "mandate signs in as a new device named mandate. Give your recovery key (Element: Settings › Encryption) " +
		"to read older encrypted messages, or leave it empty to verify mandate from another device you are signed in on.",
	Fields: []connector.Field{
		{Name: "homeserver", Label: "Homeserver URL, if not your Matrix ID's server", Kind: connector.TextField},
		{Name: "user_id", Label: "Matrix ID, e.g. @alice:matrix.org", Kind: connector.TextField, Required: true},
		{Name: "login", Label: "Sign in with", Kind: connector.ChoiceField, Required: true, Options: []connector.Option{
			{Value: "password", Label: "Password, used once and not stored"},
			{Value: "token", Label: "Access token of a new device, for servers without password login"},
		}},
		{Name: "secret", Label: "Password or access token", Kind: connector.SecretField, Required: true},
		{Name: "recovery_key", Label: "Recovery key", Kind: connector.SecretField},
	},
}

type linkInput struct {
	homeserver  string // as given, maybe empty
	user        id.UserID
	server      string // the user ID's server name
	password    string
	token       string
	recoveryKey string
}

func parseSetup(values map[string]string) (linkInput, error) {
	for k := range values {
		switch k {
		case "homeserver", "user_id", "login", "secret", "recovery_key":
		default:
			return linkInput{}, connector.Invalid("unknown field %q", k)
		}
	}
	in := linkInput{
		homeserver:  strings.TrimSpace(values["homeserver"]),
		user:        id.UserID(strings.TrimSpace(values["user_id"])),
		recoveryKey: strings.TrimSpace(values["recovery_key"]),
	}
	_, server, err := in.user.ParseAndValidateRelaxed()
	if err != nil || server == "" {
		return linkInput{}, connector.Invalid("type your full Matrix ID, e.g. @alice:matrix.org")
	}
	in.server = server
	secret := values["secret"]
	if secret == "" {
		return linkInput{}, connector.Invalid("type your password or access token")
	}
	switch values["login"] {
	case "password":
		in.password = secret
	case "token":
		in.token = strings.TrimSpace(secret)
	default:
		return linkInput{}, connector.Invalid("choose to sign in with a password or an access token")
	}
	if in.recoveryKey != "" && utils.DecodeBase58RecoveryKey(in.recoveryKey) == nil {
		return linkInput{}, connector.Invalid("that is not a recovery key; it looks like EsTc 1234 …, 48 characters in groups of four")
	}
	return in, nil
}

// Setup asks for the login on step "login", then links with the recovery
// key at once, or else verifies the new device from another one: step
// "verify" waits for the other device, step "compare" asks whether the emoji
// match.
func (c *mxConnector) Setup(ctx context.Context, step string, values map[string]string) (connector.Step, error) {
	switch step {
	case "", "login", "verify":
	case "compare":
		if answer, ok := values["confirm"]; ok {
			if len(values) != 1 {
				return connector.Step{}, connector.Invalid("confirm comes alone")
			}
			return c.answer(ctx, answer)
		}
	default:
		return connector.Step{}, connector.Invalid("unknown step %q", step)
	}
	if step, err, ok := c.verifyingStep(); ok {
		return step, err
	}
	c.mu.Lock()
	linked := c.acct != nil
	c.mu.Unlock()
	switch {
	case linked:
		return connector.Step{}, errLinked
	case step != "login" || len(values) == 0:
		return loginStep, nil
	}
	in, err := parseSetup(values)
	if err != nil {
		return connector.Step{}, err
	}
	if !c.setup.TryLock() {
		return connector.Step{}, &connector.Error{Status: http.StatusConflict, Message: "linking already in progress"}
	}
	defer c.setup.Unlock()
	c.mu.Lock()
	linked = c.acct != nil || c.verifying != nil
	c.mu.Unlock()
	if linked {
		return connector.Step{}, errLinked
	}
	sess, err := c.login(ctx, in)
	if err != nil {
		return connector.Step{}, err
	}
	a, err := c.openNew(ctx, sess)
	if err == nil && in.recoveryKey != "" {
		if err = a.recover(ctx, in.recoveryKey); err == nil {
			err = c.commit(a)
		}
		if err == nil {
			return linkedStep(a.sess), nil
		}
	}
	if err == nil {
		var step connector.Step
		if step, err = c.startVerifying(ctx, a); err == nil {
			return step, nil
		}
	}
	c.abandon(a, sess)
	return connector.Step{}, err
}

var errLinked = &connector.Error{Status: http.StatusConflict, Message: "already linked"}

func linkedStep(sess session) connector.Step {
	s := connector.Step{Title: "Matrix linked", Done: true}
	if sess.NoBackup {
		s.Text = noBackupDetail
	}
	return s
}

// login signs in as a new device, with the password or with an access
// token made for one.
func (c *mxConnector) login(ctx context.Context, in linkInput) (session, error) {
	hs, err := c.homeserver(ctx, in)
	if err != nil {
		return session{}, err
	}
	if in.token != "" {
		return c.loginToken(ctx, hs, in)
	}
	cli, err := newClient(hs, "", "", c.http, c.zl)
	if err != nil {
		return session{}, connector.Invalid("invalid homeserver URL: %v", err)
	}
	flows, err := cli.GetLoginFlows(ctx)
	if err != nil {
		return session{}, upstream("reach the homeserver at "+hs, err)
	}
	if !flows.HasFlow(mautrix.AuthTypePassword) {
		return session{}, connector.Invalid("this homeserver does not offer password login; sign in with an access token")
	}
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: in.user.String()},
		Password:                 in.password,
		InitialDeviceDisplayName: deviceName,
	})
	switch {
	case errors.Is(err, mautrix.MForbidden):
		return session{}, connector.Invalid("wrong Matrix ID or password")
	case errors.Is(err, mautrix.MUserDeactivated):
		return session{}, connector.Invalid("this account is deactivated")
	case errors.Is(err, mautrix.MLimitExceeded):
		return session{}, &connector.Error{Status: http.StatusTooManyRequests, Message: "too many login attempts; try again later"}
	case err != nil:
		return session{}, upstream("log in", err)
	}
	return session{
		Homeserver: hs, UserID: resp.UserID, DeviceID: resp.DeviceID, AccessToken: resp.AccessToken,
		PickleKey: []byte(rand.Text()),
	}, nil
}

// loginToken takes over the device of an access token. The device must be
// new: one that uploaded keys already holds them elsewhere, and its messages
// could not be decrypted here.
func (c *mxConnector) loginToken(ctx context.Context, hs string, in linkInput) (session, error) {
	cli, err := newClient(hs, "", in.token, c.http, c.zl)
	if err != nil {
		return session{}, connector.Invalid("invalid homeserver URL: %v", err)
	}
	who, err := cli.Whoami(ctx)
	switch {
	case errors.Is(err, mautrix.MUnknownToken):
		return session{}, connector.Invalid("the homeserver does not know this access token")
	case err != nil:
		return session{}, upstream("check the access token", err)
	case who.UserID != in.user:
		return session{}, connector.Invalid("this access token belongs to %s", who.UserID)
	case who.DeviceID == "":
		return session{}, connector.Invalid("this access token has no device")
	}
	cli.UserID, cli.DeviceID = who.UserID, who.DeviceID
	keys, err := cli.QueryKeys(ctx, &mautrix.ReqQueryKeys{DeviceKeys: mautrix.DeviceKeysRequest{who.UserID: {who.DeviceID}}})
	if err != nil {
		return session{}, upstream("look up the device's keys", err)
	}
	if _, ok := keys.DeviceKeys[who.UserID][who.DeviceID]; ok {
		return session{}, connector.Invalid("the device of this access token is in use already; make a token for a new device")
	}
	if err := cli.SetDeviceInfo(ctx, who.DeviceID, &mautrix.ReqDeviceInfo{DisplayName: deviceName}); err != nil {
		c.log.Warn("name the device", "err", err)
	}
	return session{
		Homeserver: hs, UserID: who.UserID, DeviceID: who.DeviceID, AccessToken: in.token,
		PickleKey: []byte(rand.Text()),
	}, nil
}

// openNew opens fresh crypto state for the logged-in device and uploads its
// keys.
func (c *mxConnector) openNew(ctx context.Context, sess session) (*account, error) {
	// Left over from a link that crashed halfway.
	if err := removeCryptoDB(c.cryptoPath()); err != nil {
		return nil, err
	}
	a, err := openAccount(ctx, sess, c.cryptoPath(), c.http, c.zl)
	if err != nil {
		return nil, err
	}
	if err := a.mach.ShareKeys(ctx, -1); err != nil {
		return a, upstream("upload device keys", err)
	}
	return a, nil
}

// commit stores the session and starts syncing a as the linked account.
func (c *mxConnector) commit(a *account) error {
	b, err := json.Marshal(a.sess)
	if err == nil {
		err = c.session.Store(b)
	}
	if err != nil {
		return fmt.Errorf("store session: %w", err)
	}
	c.mu.Lock()
	c.acct, c.loggedOut = a, false
	c.mu.Unlock()
	c.run(a)
	c.log.Info("linked", "user", a.sess.UserID, "device", a.sess.DeviceID, "backup", !a.sess.NoBackup)
	return nil
}

// abandon logs out a device that did not get linked, best effort, and
// deletes its local state. a may be nil.
func (c *mxConnector) abandon(a *account, sess session) {
	if a != nil {
		a.close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cli, err := newClient(sess.Homeserver, sess.UserID, sess.AccessToken, c.http, c.zl); err == nil {
		if _, err := cli.Logout(ctx); err != nil {
			c.log.Warn("log out the device that was not linked", "device", sess.DeviceID, "err", err)
		}
	}
	if err := c.forget(); err != nil {
		c.log.Error("forget the device that was not linked", "err", err)
	}
}

var errWrongRecoveryKey = connector.Invalid("wrong recovery key")

// recover unlocks secret storage with the recovery key, cross-signs this
// device, and imports the room keys in the server-side backup.
func (a *account) recover(ctx context.Context, recoveryKey string) error {
	keyID, meta, err := a.mach.SSSS.GetDefaultKeyData(ctx)
	if errors.Is(err, mautrix.MNotFound) || errors.Is(err, ssss.ErrNoDefaultKeyID) {
		return connector.Invalid("this account has no recovery set up; set it up in Element under Settings → Encryption")
	} else if err != nil {
		return upstream("read secret storage", err)
	}
	key, err := meta.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil && !errors.Is(err, ssss.ErrUnverifiableKey) {
		return errWrongRecoveryKey
	}
	if err := a.mach.FetchCrossSigningKeysFromSSSS(ctx, key); err != nil {
		if errors.Is(err, ssss.ErrKeyDataMACMismatch) || errors.Is(err, ssss.ErrNotEncryptedForKey) {
			return errWrongRecoveryKey
		}
		return upstream("read cross-signing keys", err)
	}
	if err := a.mach.SignOwnDevice(ctx, a.mach.OwnIdentity()); err != nil {
		return upstream("cross-sign this device", err)
	}
	if err := a.mach.SignOwnMasterKey(ctx); err != nil {
		return upstream("sign the master key", err)
	}
	raw, err := a.mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if errors.Is(err, mautrix.MNotFound) {
		return nil // no key backup to restore
	} else if err != nil {
		return upstream("read the key backup key", err)
	}
	bk, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return fmt.Errorf("key backup key: %w", err)
	}
	if _, err := a.mach.DownloadAndStoreLatestKeyBackup(ctx, bk); err != nil && !errors.Is(err, mautrix.MNotFound) {
		return upstream("restore the key backup", err)
	}
	return nil
}

// homeserver is the client API base URL: the one given, or the one the
// server's .well-known names.
func (c *mxConnector) homeserver(ctx context.Context, in linkInput) (string, error) {
	raw := strings.TrimSuffix(in.homeserver, "/")
	if raw == "" {
		raw = in.server
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Path != "" {
			return "", connector.Invalid("homeserver must be a URL such as https://matrix.org")
		}
		if wk, err := mautrix.DiscoverClientAPIWithClient(ctx, c.http, u.Host); err == nil && wk != nil && wk.Homeserver.BaseURL != "" {
			raw = wk.Homeserver.BaseURL
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", connector.Invalid("homeserver must be a URL such as https://matrix.org")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname())) {
		return "", connector.Invalid("homeserver must use https")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Unlink logs the device out, best effort, and deletes its keys. Stored
// messages stay.
func (c *mxConnector) Unlink(ctx context.Context) error {
	c.setup.Lock()
	defer c.setup.Unlock()
	c.endVerifying(verifyFailure{msg: "unlinked", cancel: event.VerificationCancelCodeUser})
	c.mu.Lock()
	a := c.acct
	c.acct, c.loggedOut = nil, false
	c.mu.Unlock()
	if a != nil {
		a.stop()
		if _, err := a.cli.Logout(ctx); err != nil {
			c.log.Warn("log out from the homeserver failed; forgetting the device anyway", "err", err)
		}
		a.close()
	}
	if err := c.forget(); err != nil {
		return fmt.Errorf("forget session: %w", err)
	}
	c.log.Info("unlinked")
	return nil
}

// upstream is a homeserver failure, with the Matrix error code if there is one.
func upstream(what string, err error) error {
	var he mautrix.HTTPError
	if errors.As(err, &he) && he.RespError != nil {
		return &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("%s: %s %s", what, he.RespError.ErrCode, he.RespError.Err)}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &connector.Error{Status: http.StatusBadGateway, Message: fmt.Sprintf("%s: %v", what, err)}
}
