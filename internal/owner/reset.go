package owner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/peer"
)

// Reset stops mandate, forgets the paired devices with the key logs,
// identities and grants, and makes a new setup password. Connectors keep
// their spent approvals, so nothing signed before runs again. Linked
// accounts stay linked, unless unlink: then each connector logs out at its
// service where it can, while it still runs, and forgets its account and
// store.
func Reset(ctx context.Context, cfg *config.Config, out io.Writer, unlink bool) error {
	conns := map[string]*connector.Client{}
	for name := range cfg.Connectors {
		conns[name] = connector.Dial(name, config.ConnectorSocket(name))
	}
	var accounts []account
	if err := asOwner(func() { accounts = survey(ctx, conns, unlink) }); err != nil {
		return err
	}
	restart, err := stopServices(ctx, cfg)
	if err != nil {
		return err
	}
	password, err := wipe()
	if err == nil && unlink {
		for _, a := range accounts {
			if err = forget(config.ConnectorDir(a.name), a.loggedOut); err != nil {
				break
			}
		}
	}
	if restart != nil {
		if rerr := restart(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("start mandate.target: %w", rerr))
		}
	}
	if err != nil {
		return err
	}
	var linked, notOut []string
	for _, a := range accounts {
		switch {
		case !unlink && a.linked:
			linked = append(linked, a.name)
		case unlink && (a.linked || !a.reached) && !a.loggedOut:
			notOut = append(notOut, a.name)
		}
	}
	var more [][2]string
	if len(linked) > 0 {
		more = append(more, [2]string{"Still linked", strings.Join(linked, ", ")})
	}
	if len(notOut) > 0 {
		more = append(more, [2]string{"Not logged out", strings.Join(notOut, ", ")})
	}
	printSetup(ctx, cfg, out, password, more...)
	return nil
}

// account is what reset learns of a connector's account while it runs.
type account struct {
	name      string
	reached   bool // it answered
	linked    bool // its health said so
	loggedOut bool // it unlinked, logging out at its service
}

// survey asks every connector at once whether it is linked and, if unlink,
// unlinks those that can be. Sorted by name.
func survey(ctx context.Context, conns map[string]*connector.Client, unlink bool) []account {
	out := make([]account, 0, len(conns))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, c := range conns {
		wg.Go(func() {
			a := account{name: name}
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			h, err := c.Health(hctx)
			cancel()
			a.reached, a.linked = err == nil, err == nil && h.State != connector.NeedsSetup
			if unlink && a.linked {
				sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				spec, err := c.Spec(sctx)
				cancel()
				if err == nil && spec.Unlink {
					uctx, cancel := context.WithTimeout(ctx, time.Minute)
					a.loggedOut = c.Unlink(uctx) == nil
					cancel()
				}
			}
			mu.Lock()
			out = append(out, a)
			mu.Unlock()
		})
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b account) int { return strings.Compare(a.name, b.name) })
	return out
}

// forget deletes a stopped connector's store and, unless it logged out
// itself, its session in private/. Its spent approvals stay.
func forget(dir string, loggedOut bool) error {
	for _, f := range []string{"data.db", "data.db-wal", "data.db-shm", "data.db-journal"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete the store: %w", err)
		}
	}
	if loggedOut {
		return nil
	}
	private := filepath.Join(dir, "private")
	entries, err := os.ReadDir(private)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.Name() == "spent" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(private, e.Name())); err != nil {
			return fmt.Errorf("delete the session: %w", err)
		}
	}
	return nil
}

// asOwner runs f with the effective group of the owner side, which
// connectors let unlink. Reset runs as root, while the units run.
func asOwner(f func()) error {
	gid, err := peer.LookupGID("mandate-owner")
	if err != nil {
		return fmt.Errorf("group mandate-owner: %w", err)
	}
	was := os.Getegid()
	if err := syscall.Setegid(int(gid)); err != nil {
		return fmt.Errorf("act as mandate-owner: %w", err)
	}
	defer func() {
		if err := syscall.Setegid(was); err != nil {
			panic(err) // the rest of reset must not run as another group
		}
	}()
	f()
	return nil
}

// stopServices stops mandate.target and returns how to start it again. With
// no systemd or no target it only makes sure nothing of mandate runs.
func stopServices(ctx context.Context, cfg *config.Config) (func() error, error) {
	// systemctl waits only for the jobs it was asked for, and stopping a
	// target finishes before the units that are PartOf it, so name them too.
	if _, err := exec.LookPath("systemctl"); err == nil &&
		exec.CommandContext(ctx, "systemctl", "stop", "mandate.target", "mandate-*.service").Run() == nil {
		return exec.CommandContext(ctx, "systemctl", "start", "mandate.target").Run, nil
	}
	socks := []string{config.Socket("owner"), config.Socket("gateway")}
	for name := range cfg.Connectors {
		socks = append(socks, config.ConnectorSocket(name))
	}
	for _, sock := range socks {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", sock); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s answers: stop mandate's services first", sock)
		}
	}
	return nil, nil
}

// wipe empties every key log and the owner side's state but never-share
// and activity, and makes a new setup password.
func wipe() (string, error) {
	dir := config.StateDir("owner")
	old, err := loadState(dir)
	if err != nil {
		return "", err
	}
	st := newState()
	st.NeverShare = old.NeverShare
	b, err := json.Marshal(&st)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeOwned(stateFile(dir), b, 0o600); err != nil {
		return "", fmt.Errorf("reset owner state: %w", err)
	}
	logs, err := filepath.Glob(filepath.Join(config.ConnectorDir("*"), "private", "keylog.json"))
	if err != nil {
		return "", err
	}
	for _, f := range logs {
		if err := os.Remove(f); err != nil {
			return "", fmt.Errorf("reset connector key log: %w", err)
		}
	}
	// All the gateway keeps derives from the live set the owner side pushes.
	gw := config.StateDir("gateway")
	entries, err := os.ReadDir(gw)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(gw, e.Name())); err != nil {
			return "", fmt.Errorf("reset gateway state: %w", err)
		}
	}
	for _, f := range []string{setupFile(dir), config.SetupPasswordFile()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return setupPassword(dir)
}
