package owner

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/template"

	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/peer"
)

const (
	// Where install.sh puts the bundled connectors.
	bundledDir = "/usr/local/lib/mandate/connectors"
	unitDir    = "/etc/systemd/system"
)

//go:embed systemd.tmpl
var unitText string

var units = template.Must(template.New("units").Funcs(template.FuncMap{"quote": execQuote}).Parse(unitText))

type connectorUnit struct {
	Exe, Name, Command, Config string
}

// Install readies a host that the NixOS module doesn't manage for Setup: it
// makes the three groups, the default config unless there is one, and
// mandate's systemd units, and starts them. On NixOS, or once mandate.target
// exists, it does nothing.
func Install(ctx context.Context) error {
	if _, err := os.Stat("/etc/NIXOS"); err == nil {
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("mandate runs under systemd, and there is no systemctl")
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "show", "--property=LoadState", "--value", "mandate.target").Output()
	if strings.TrimSpace(string(out)) == "loaded" {
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("run it as root: sudo mandate setup")
	}
	for _, g := range []string{"mandate-front", "mandate-gateway", "mandate-owner"} {
		if err := run(ctx, "groupadd", "--system", "--force", g); err != nil {
			return err
		}
	}
	if err := writeDefaultConfig(); err != nil {
		return fmt.Errorf("write %s: %w", config.File(), err)
	}
	cfg, err := config.Load(config.File())
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	conns, err := connectorConfigs(ctx, exe, cfg)
	if err != nil {
		return err
	}

	data := struct {
		Exe        string
		Connectors []connectorUnit
	}{exe, conns}
	// mandate.target goes last, so that a setup cut short writes them all again.
	var names []string
	for _, c := range conns {
		names = append(names, "mandate-"+c.Name+".service")
		if err := writeUnit(names[len(names)-1], "connector", c); err != nil {
			return err
		}
	}
	for _, name := range []string{"mandate-front.service", "mandate-gateway.service", "mandate-owner.service", "mandate.target"} {
		names = append(names, name)
		if err := writeUnit(name, name, data); err != nil {
			return err
		}
	}
	if err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", append([]string{"enable", "--quiet", "--now"}, names...)...)
}

// writeDefaultConfig writes the owner's default mandate.toml unless one is
// there: the one port, mandate's own TLS key, and every connector install.sh
// put in bundledDir.
func writeDefaultConfig() error {
	file := config.File()
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(bundledDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var b strings.Builder
	b.WriteString("listen = \":8443\"\n\n[tls]\nmode = \"own\"\n")
	for _, e := range entries {
		if e.Type().IsRegular() {
			fmt.Fprintf(&b, "\n[connectors.%s]\ncommand = %q\n", e.Name(), filepath.Join(bundledDir, e.Name()))
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return writeFile(file, []byte(b.String()), 0o644)
}

// connectorConfigs writes each connector's config as the JSON file its unit
// passes to --config, and has the connector check it.
func connectorConfigs(ctx context.Context, exe string, cfg *config.Config) ([]connectorUnit, error) {
	dir := filepath.Join(filepath.Dir(config.File()), "connectors")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var conns []connectorUnit
	for _, name := range slices.Sorted(maps.Keys(cfg.Connectors)) {
		c := cfg.Connectors[name]
		b, err := json.Marshal(c.Config)
		if err != nil {
			return nil, err
		}
		file := filepath.Join(dir, name+".json")
		if err := writeFile(file, append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
		if err := run(ctx, c.Command, "--check-config", file); err != nil {
			return nil, fmt.Errorf("connector %s: %w", name, err)
		}
		conns = append(conns, connectorUnit{Exe: exe, Name: name, Command: c.Command, Config: file})
	}
	return conns, nil
}

func writeUnit(file, tmpl string, data any) error {
	var b bytes.Buffer
	if err := units.ExecuteTemplate(&b, tmpl, data); err != nil {
		return err
	}
	return writeFile(filepath.Join(unitDir, file), b.Bytes(), 0o644)
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// execQuote quotes a word of an Exec line where systemd would split or
// expand it.
func execQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-") == "" {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// StoreACL lets the gateway's group read connector name's store, run as root
// before the connector starts: the directory, by its default ACL what the
// connector creates in it, and a store that is there already, from an older
// layout or after systemd's chown dropped its ACLs. private/ is left alone:
// its mask must stay empty. Symlinks the connector might plant are skipped.
// The unit's own view of the directory is idmapped, where the gateway's
// group has no ID, so this goes through PID 1's root.
func StoreACL(name string) error {
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("bad connector name %q", name)
	}
	gid, err := peer.LookupGID("mandate-gateway")
	if err != nil {
		return err
	}
	dir := filepath.Join("/proc/1/root/var/lib/private/mandate/connectors", name)
	if err := grantRead(dir, gid); err != nil {
		return err
	}
	for _, f := range []string{"data.db", "data.db-journal"} {
		err := grantRead(filepath.Join(dir, f), gid)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ELOOP) {
			return err
		}
	}
	return nil
}

// POSIX ACLs as the kernel keeps them in xattrs.
const (
	aclAccess  = "system.posix_acl_access"
	aclDefault = "system.posix_acl_default"
	aclVersion = 2

	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
	aclNoID     = ^uint32(0)
)

type aclEntry struct {
	Tag, Perm uint16
	ID        uint32
}

// grantRead adds the group gid to path's ACL as setfacl -P -m does: r-x on
// a directory, also in its default ACL, and r on a regular file. Anything
// else, and a symlink, is left alone.
func grantRead(path string, gid uint32) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return nil
	}
	// The open file, so that nothing is swapped in under the name.
	fd := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	perm := uint16(4)
	if fi.IsDir() {
		perm = 5
	}
	access, err := getACL(fd, aclAccess)
	if err != nil {
		return err
	}
	if access == nil {
		m := uint16(fi.Mode().Perm())
		access = []aclEntry{{aclUserObj, m >> 6 & 7, aclNoID}, {aclGroupObj, m >> 3 & 7, aclNoID}, {aclOther, m & 7, aclNoID}}
	}
	if err := setACL(fd, aclAccess, withGroup(access, gid, perm)); err != nil {
		return err
	}
	if !fi.IsDir() {
		return nil
	}
	def, err := getACL(fd, aclDefault)
	if err != nil {
		return err
	}
	if def == nil {
		// As setfacl does, start from the access ACL's base entries.
		for _, e := range access {
			if e.Tag == aclUserObj || e.Tag == aclGroupObj || e.Tag == aclOther {
				def = append(def, e)
			}
		}
	}
	return setACL(fd, aclDefault, withGroup(def, gid, perm))
}

// getACL reads an ACL, or nil if there is none.
func getACL(path, attr string) ([]aclEntry, error) {
	n, err := syscall.Getxattr(path, attr, nil)
	if errors.Is(err, syscall.ENODATA) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", attr, path, err)
	}
	b := make([]byte, n)
	if n, err = syscall.Getxattr(path, attr, b); err != nil {
		return nil, fmt.Errorf("read %s of %s: %w", attr, path, err)
	}
	if n < 4 || (n-4)%8 != 0 || binary.LittleEndian.Uint32(b) != aclVersion {
		return nil, fmt.Errorf("%s of %s: not an ACL", attr, path)
	}
	acl := make([]aclEntry, (n-4)/8)
	if err := binary.Read(bytes.NewReader(b[4:n]), binary.LittleEndian, acl); err != nil {
		return nil, err
	}
	return acl, nil
}

func setACL(path, attr string, acl []aclEntry) error {
	b := binary.LittleEndian.AppendUint32(nil, aclVersion)
	b, err := binary.Append(b, binary.LittleEndian, acl)
	if err != nil {
		return err
	}
	if err := syscall.Setxattr(path, attr, b, 0); err != nil {
		return fmt.Errorf("set %s of %s: %w", attr, path, err)
	}
	return nil
}

// withGroup sets the group gid's entry and recomputes the mask, in the
// order the kernel wants.
func withGroup(acl []aclEntry, gid uint32, perm uint16) []aclEntry {
	out := []aclEntry{{aclGroup, perm, gid}}
	mask := perm
	for _, e := range acl {
		switch {
		case e.Tag == aclMask || e.Tag == aclGroup && e.ID == gid:
			continue
		case e.Tag == aclUser || e.Tag == aclGroupObj || e.Tag == aclGroup:
			mask |= e.Perm
		}
		out = append(out, e)
	}
	out = append(out, aclEntry{aclMask, mask, aclNoID})
	slices.SortFunc(out, func(a, b aclEntry) int {
		return cmp.Or(cmp.Compare(a.Tag, b.Tag), cmp.Compare(a.ID, b.ID))
	})
	return out
}
