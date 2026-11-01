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

var setupParams = []connector.Param{
	{Name: "homeserver", Doc: "Homeserver URL, e.g. https://matrix.org; empty uses your user ID's server"},
	{Name: "user_id", Doc: "Your Matrix ID, e.g. @alice:matrix.org", Required: true},
	{Name: "password", Doc: "Your account password; used once to log in, not stored", Required: true, Secret: true},
	{Name: "recovery_key", Secret: true, Doc: "Only if you have no other device signed in: your recovery key (Element: Settings → Encryption). " +
		"Without it, you verify mandate from Element on another device."},
}

type linkInput struct {
	homeserver  string // as given, maybe empty
	user        id.UserID
	server      string // the user ID's server name
	password    string
	recoveryKey string
}

func parseSetup(args map[string]string) (linkInput, error) {
	for k := range args {
		switch k {
		case "homeserver", "user_id", "password", "recovery_key":
		default:
			return linkInput{}, connector.Invalid("unknown arg %q", k)
		}
	}
	in := linkInput{
		homeserver:  strings.TrimSpace(args["homeserver"]),
		user:        id.UserID(strings.TrimSpace(args["user_id"])),
		password:    args["password"],
		recoveryKey: strings.TrimSpace(args["recovery_key"]),
	}
	_, server, err := in.user.ParseAndValidateRelaxed()
	if err != nil || server == "" {
		return linkInput{}, connector.Invalid("user_id must be a full Matrix ID, e.g. @alice:matrix.org")
	}
	in.server = server
	if in.password == "" {
		return linkInput{}, connector.Invalid("password is required")
	}
	if in.recoveryKey != "" && utils.DecodeBase58RecoveryKey(in.recoveryKey) == nil {
		return linkInput{}, connector.Invalid("that is not a recovery key; it looks like EsTc 1234 …, 48 characters in groups of four")
	}
	return in, nil
}

// Setup links with the recovery key at once, or else starts verifying the new
// device from another one, which later calls follow.
func (c *mxConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	if answer, ok := args[connector.SetupConfirm]; ok {
		if len(args) != 1 {
			return connector.SetupStep{}, connector.Invalid("confirm comes alone")
		}
		return c.answer(ctx, answer)
	}
	if step, err, ok := c.verifyingStep(); ok {
		return step, err
	}
	in, err := parseSetup(args)
	if err != nil {
		return connector.SetupStep{}, err
	}
	if !c.setup.TryLock() {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "linking already in progress"}
	}
	defer c.setup.Unlock()
	c.mu.Lock()
	linked := c.acct != nil || c.verifying != nil
	c.mu.Unlock()
	if linked {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	sess, err := c.login(ctx, in)
	if err != nil {
		return connector.SetupStep{}, err
	}
	a, err := c.openNew(ctx, sess)
	if err == nil && in.recoveryKey != "" {
		if err = a.recover(ctx, in.recoveryKey); err == nil {
			err = c.commit(a)
		}
		if err == nil {
			return connector.SetupStep{Done: true}, nil
		}
	}
	if err == nil {
		var step connector.SetupStep
		if step, err = c.startVerifying(ctx, a); err == nil {
			return step, nil
		}
	}
	c.abandon(a, sess)
	return connector.SetupStep{}, err
}

// login logs in as a new device.
func (c *mxConnector) login(ctx context.Context, in linkInput) (session, error) {
	hs, err := c.homeserver(ctx, in)
	if err != nil {
		return session{}, err
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
		return session{}, connector.Invalid("this homeserver does not offer password login")
	}
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: in.user.String()},
		Password:                 in.password,
		InitialDeviceDisplayName: deviceName,
	})
	switch {
	case errors.Is(err, mautrix.MForbidden):
		return session{}, connector.Invalid("wrong user ID or password")
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
