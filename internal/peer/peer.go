// Package peer serves HTTP on a Unix socket that anyone may connect to and
// tells callers apart by the primary group the kernel recorded for them.
package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Listen listens on a Unix socket at path with mode 0666, replacing a stale
// socket. Anyone may connect; Allow decides what each caller may call.
func Listen(path string) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type gidKey struct{}

// ConnContext records the caller's primary group, for http.Server.ConnContext.
// A connection without one is refused by every Allow.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	gid, err := peerGID(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, gidKey{}, gid)
}

// LogPath is p as logs may show it: a setup link's password is left out.
func LogPath(p string) string {
	if strings.HasPrefix(p, "/setup/") {
		return "/setup/…"
	}
	return p
}

// Allow admits callers whose primary group is one of gids to h and answers
// anyone else 403.
func Allow(log *slog.Logger, h http.Handler, gids ...uint32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gid, ok := r.Context().Value(gidKey{}).(uint32)
		if !ok || !slices.Contains(gids, gid) {
			log.Warn("caller refused", "gid", gid, "known", ok, "method", r.Method, "path", LogPath(r.URL.Path))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "this caller may not use " + r.Method + " " + r.URL.Path})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// peerGID is the primary group of the process at the other end of a Unix
// socket, as the kernel recorded it at connect.
func peerGID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("%T is not a Unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Gid, nil
}

// LookupGID resolves a group name, or takes a numeric gid as is: without
// cgo, os/user reads only /etc/group, which lacks systemd's dynamic groups.
func LookupGID(group string) (uint32, error) {
	if gid, err := strconv.ParseUint(group, 10, 32); err == nil {
		return uint32(gid), nil
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	return uint32(gid), err
}
