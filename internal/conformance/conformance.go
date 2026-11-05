// Package conformance is the connector conformance kit of
// docs/spec/connector.md: it runs a connector program with a fresh state
// directory and checks what needs no account.
package conformance

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
)

const usage = "usage: mandate conformance PROGRAM [--config FILE] [--store FILE]"

// name is the --name the kit runs the program under: not the program's own
// name, so a spec that ignores --name shows.
const name = "conformance"

type kit struct {
	ctx     context.Context
	out     io.Writer
	program string
	config  string
	dir     string
	// prefix starts the names of the program's own store tables.
	prefix string

	checks, failed, skipped int
}

// Run runs the kit on a connector program and fails if any check does.
func Run(ctx context.Context, args []string, out io.Writer) error {
	var program string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		program, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("conformance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	config := flags.String("config", "", "")
	storePath := flags.String("store", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 || program == "" {
		return errors.New(usage)
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "mandate-conformance-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Mkdir(filepath.Join(dir, "state"), 0o700); err != nil {
		return err
	}
	k := &kit{
		ctx: ctx, out: out, program: path, config: *config, dir: dir,
		prefix: strings.TrimPrefix(filepath.Base(path), "mandate-") + "_",
	}
	fmt.Fprintf(out, "%s\n", path)

	k.checkConfig()
	me := uint32(os.Getegid())
	k.run(me+1, me, "owner", k.asOwner)
	k.run(me, me+1, "gateway", k.asGateway)
	k.run(me+1, me+2, "stranger", k.asStranger)
	if *storePath != "" {
		k.storeRules(*storePath)
	}

	fmt.Fprintf(out, "%d checks: %d failed, %d skipped\n", k.checks, k.failed, k.skipped)
	if k.failed > 0 {
		return fmt.Errorf("%s fails %d of %d checks", filepath.Base(path), k.failed, k.checks)
	}
	return nil
}

// skip is a check the connector answered in a way that leaves nothing to
// check, such as setup while the service is out of reach.
type skip string

func (s skip) Error() string { return string(s) }

func (k *kit) check(what string, err error) {
	k.checks++
	var s skip
	switch {
	case err == nil:
		fmt.Fprintf(k.out, "ok    %s\n", what)
	case errors.As(err, &s):
		k.skipped++
		fmt.Fprintf(k.out, "skip  %s: %v\n", what, err)
	default:
		k.failed++
		fmt.Fprintf(k.out, "FAIL  %s: %v\n", what, err)
	}
}

func (k *kit) checkConfig() {
	base, file := []byte("{}"), k.config
	if file != "" {
		var err error
		if base, err = os.ReadFile(file); err != nil {
			k.check("--check-config takes the config", err)
			return
		}
	} else {
		file = filepath.Join(k.dir, "config.json")
		if err := os.WriteFile(file, base, 0o600); err != nil {
			k.check("--check-config takes the config", err)
			return
		}
	}
	k.check("--check-config takes the config", k.checkConfigFile(file, true))

	var cfg map[string]any
	if err := json.Unmarshal(base, &cfg); err != nil || cfg == nil {
		k.check("--check-config refuses an unknown key", errors.New("the config is not a JSON object"))
		return
	}
	cfg["conformance_unknown_key"] = true
	b, _ := json.Marshal(cfg)
	unknown := filepath.Join(k.dir, "unknown.json")
	err := os.WriteFile(unknown, b, 0o600)
	if err == nil {
		err = k.checkConfigFile(unknown, false)
	}
	k.check("--check-config refuses an unknown key", err)
}

func (k *kit) checkConfigFile(file string, valid bool) error {
	ctx, cancel := context.WithTimeout(k.ctx, 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, k.program, "--check-config", file)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exit):
		return err
	case valid && err != nil:
		return fmt.Errorf("%w: %s", err, lastLine(stderr.Bytes()))
	case !valid && err == nil:
		return errors.New("exits 0")
	case !valid && len(bytes.TrimSpace(stderr.Bytes())) == 0:
		return errors.New("refuses it without a message on stderr")
	}
	return nil
}

// proc is a running connector and a client on its socket.
type proc struct {
	cmd    *exec.Cmd
	stderr bytes.Buffer // read only after exited
	exited chan struct{}
	err    error
	http   *http.Client
}

// run starts the program under the two caller groups and runs checks on it.
func (k *kit) run(gateway, owner uint32, caller string, checks func(*proc)) {
	p, err := k.start(gateway, owner)
	if err != nil {
		k.check("starts with "+caller+" as the caller", err)
		return
	}
	defer p.stop()
	checks(p)
}

func (k *kit) start(gateway, owner uint32) (*proc, error) {
	socket := filepath.Join(k.dir, "socket")
	args := []string{
		"--name", name, "--socket", socket, "--state", filepath.Join(k.dir, "state"),
		"--gateway-group", strconv.FormatUint(uint64(gateway), 10),
		"--owner-group", strconv.FormatUint(uint64(owner), 10),
	}
	if k.config != "" {
		args = append(args, "--config", k.config)
	}
	p := &proc{cmd: exec.CommandContext(k.ctx, k.program, args...), exited: make(chan struct{})}
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	var d net.Dialer
	p.http = &http.Client{
		Timeout: 90 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		}},
	}
	deadline := time.After(30 * time.Second)
	for {
		if c, err := d.DialContext(k.ctx, "unix", socket); err == nil {
			c.Close()
			return p, nil
		}
		select {
		case <-p.exited:
			return nil, fmt.Errorf("exited before serving (%w): %s", p.err, lastLine(p.stderr.Bytes()))
		case <-deadline:
			p.stop()
			return nil, errors.New("not serving on its socket after 30 seconds")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *proc) stop() {
	p.http.CloseIdleConnections()
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

func lastLine(b []byte) string {
	b = bytes.TrimSpace(b)
	return string(b[bytes.LastIndexByte(b, '\n')+1:])
}

// call sends body as JSON, or no body if it is nil.
func (k *kit) call(p *proc, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(k.ctx, method, "http://connector"+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// get calls and wants a 200.
func (k *kit) get(p *proc, method, path string, body any) ([]byte, error) {
	status, b, err := k.call(p, method, path, body)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("%s %s answered %d: %s", method, path, status, bytes.TrimSpace(b))
	}
	return b, err
}

// refused wants a refusal as the protocol has it: not 2xx, with an error.
func refused(status int, b []byte, err error) error {
	if err != nil {
		return err
	}
	if status/100 == 2 {
		return fmt.Errorf("answered %d: %s", status, bytes.TrimSpace(b))
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error == "" {
		return fmt.Errorf("refused with %d but no {\"error\": …} body", status)
	}
	return nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

var routes = []struct {
	method, path   string
	gateway, owner bool
}{
	{"GET", "/spec", true, true},
	{"GET", "/health", true, true},
	{"GET", "/keys", true, true},
	{"POST", "/prepare", true, true},
	{"POST", "/read", true, false},
	{"POST", "/media", true, false},
	{"POST", "/setup", false, true},
	{"POST", "/unlink", false, true},
	{"POST", "/execute", false, true},
	{"PUT", "/keys", false, true},
	{"GET", "/containers", false, true},
}

// refusesCaller wants a 403 on every route allowed reports false for.
func (k *kit) refusesCaller(p *proc, allowed func(gateway, owner bool) bool) error {
	var errs []error
	for _, r := range routes {
		if allowed(r.gateway, r.owner) {
			continue
		}
		var body any
		switch r.method {
		case "PUT":
			body = []any{}
		case "POST":
			body = struct{}{}
		}
		status, _, err := k.call(p, r.method, r.path, body)
		if err == nil && status != http.StatusForbidden {
			err = fmt.Errorf("%s %s answered %d", r.method, r.path, status)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (k *kit) asGateway(p *proc) {
	_, err := k.get(p, "GET", "/spec", nil)
	k.check("the gateway may call GET /spec", err)
	k.check("the gateway is refused the owner's routes", k.refusesCaller(p, func(gateway, _ bool) bool { return gateway }))
}

func (k *kit) asStranger(p *proc) {
	k.check("any other caller is refused every route", k.refusesCaller(p, func(bool, bool) bool { return false }))
}

func (k *kit) asOwner(p *proc) {
	k.check("the owner is refused the gateway's routes", k.refusesCaller(p, func(_, owner bool) bool { return owner }))

	var spec connector.Spec
	b, err := k.get(p, "GET", "/spec", nil)
	if err == nil {
		spec, err = checkSpec(b)
	}
	k.check("spec", err)

	b, err = k.get(p, "GET", "/health", nil)
	if err == nil {
		err = checkHealth(b)
	}
	k.check("health", err)

	data := filepath.Join(k.dir, "state", "data.db")
	k.check("store schema and user_version", k.checkSchema(data))

	status, b, err := k.call(p, "POST", "/setup", map[string]any{"step": "", "values": map[string]string{}})
	if err == nil {
		err = checkSetup(status, b)
	}
	k.check("setup step", err)

	k.checkKeysAndExecute(p, spec, data)
}

func checkSpec(b []byte) (connector.Spec, error) {
	var s connector.Spec
	if err := decodeStrict(b, &s); err != nil {
		return s, err
	}
	if s.Protocol != connector.ProtocolVersion {
		return s, fmt.Errorf("protocol %d, not %d", s.Protocol, connector.ProtocolVersion)
	}
	if s.Name != name {
		return s, fmt.Errorf("name %q, not the --name %q", s.Name, name)
	}
	ops := make(map[string]bool)
	for _, op := range s.Ops {
		switch {
		case op.Name == "" || ops[op.Name]:
			return s, fmt.Errorf("op name %q is empty or repeated", op.Name)
		case op.Kind != connector.Read && op.Kind != connector.Write:
			return s, fmt.Errorf("op %s has kind %q, not read or write", op.Name, op.Kind)
		}
		ops[op.Name] = true
		params := make(map[string]bool)
		var containers int
		var message bool
		for _, p := range op.Params {
			if p.Name == "" || params[p.Name] {
				return s, fmt.Errorf("op %s: param name %q is empty or repeated", op.Name, p.Name)
			}
			params[p.Name] = true
			if p.Container {
				containers++
			}
			message = message || p.Message
		}
		if message && containers != 1 {
			return s, fmt.Errorf("op %s has a message param and %d container params, not one", op.Name, containers)
		}
	}
	return s, nil
}

func checkHealth(b []byte) error {
	var h struct {
		State  connector.HealthState `json:"state"`
		Detail string                `json:"detail"`
		Until  string                `json:"until"`
	}
	if err := decodeStrict(b, &h); err != nil {
		return err
	}
	states := []connector.HealthState{connector.Healthy, connector.Connecting, connector.Resting, connector.NeedsSetup, connector.Broken}
	if !slices.Contains(states, h.State) {
		return fmt.Errorf("state %q is not ok, connecting, resting, setup or down", h.State)
	}
	if _, err := time.Parse(time.RFC3339, h.Until); h.Until != "" && (err != nil || !strings.HasSuffix(h.Until, "Z")) {
		return fmt.Errorf("until %q is not an RFC 3339 UTC time", h.Until)
	}
	if h.State == connector.Resting && h.Until == "" {
		return errors.New("resting without until")
	}
	return nil
}

func checkSetup(status int, b []byte) error {
	if status != http.StatusOK {
		if err := refused(status, b, nil); err != nil {
			return err
		}
		return skip(fmt.Sprintf("answered %d (%s), no step to check", status, bytes.TrimSpace(b)))
	}
	var s connector.Step
	if err := decodeStrict(b, &s); err != nil {
		return err
	}
	switch {
	case s.Done:
		return nil
	case s.ID == "":
		return errors.New("a step without a step ID")
	case s.Poll < 0:
		return fmt.Errorf("poll %d", s.Poll)
	case len(s.Fields) == 0 && s.Confirm == "" && s.Poll == 0:
		return errors.New("a step that neither asks, confirms, polls nor is done")
	}
	names := make(map[string]bool)
	kinds := []connector.FieldKind{connector.TextField, connector.SecretField, connector.NumberField, connector.ChoiceField}
	for _, f := range s.Fields {
		switch {
		case f.Name == "" || names[f.Name]:
			return fmt.Errorf("field name %q is empty or repeated", f.Name)
		case f.Label == "":
			return fmt.Errorf("field %s has no label", f.Name)
		case !slices.Contains(kinds, f.Kind):
			return fmt.Errorf("field %s has kind %q, not text, secret, number or choice", f.Name, f.Kind)
		case (f.Kind == connector.ChoiceField) != (len(f.Options) > 0):
			return fmt.Errorf("field %s: options belong to choice fields, and choice fields need them", f.Name)
		}
		names[f.Name] = true
		for _, o := range f.Options {
			if o.Value == "" || o.Label == "" {
				return fmt.Errorf("field %s has an option without value or label", f.Name)
			}
		}
	}
	return nil
}

func newKey() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func sign(r record.Record, issued time.Time, key *ecdsa.PrivateKey) record.Envelope {
	e, err := record.New(r, issued)
	if err == nil {
		err = record.Sign(&e, key)
	}
	if err != nil {
		panic(err) // the kit's own records are valid
	}
	return e
}

func keyEntry(seq int, op string, key *ecdsa.PrivateKey, prev *record.Envelope, signer *ecdsa.PrivateKey) record.Envelope {
	k, err := record.P256Key(&key.PublicKey, name)
	if err != nil {
		panic(err)
	}
	var head string
	if prev != nil {
		h := sha256.Sum256(prev.Record)
		head = hex.EncodeToString(h[:])
	}
	now := time.Now()
	return sign(record.KeyEntry{Seq: seq, Op: op, Key: k, Prev: head, At: record.Time{Time: now}}, now, signer)
}

func (k *kit) keys(p *proc) (connector.KeysHead, error) {
	var h connector.KeysHead
	b, err := k.get(p, "GET", "/keys", nil)
	if err == nil {
		err = decodeStrict(b, &h)
	}
	return h, err
}

// checkKeysAndExecute trusts key a through a log that adds and removes b
// and adds and retires d, then offers execute approvals that each break one
// rule.
func (k *kit) checkKeysAndExecute(p *proc, spec connector.Spec, data string) {
	a, b, c, d := newKey(), newKey(), newKey(), newKey()
	e0 := keyEntry(0, "add", a, nil, a)
	e1 := keyEntry(1, "add", b, &e0, a)
	e2 := keyEntry(2, "remove", b, &e1, a)
	e3 := keyEntry(3, "add", d, &e2, a)
	e4 := keyEntry(4, "retire", d, &e3, a)
	entries := []record.Envelope{e0, e1, e2, e3, e4}
	log, err := record.NewLog(entries)
	if err != nil {
		panic(err)
	}
	n, head := log.Head()
	want := connector.KeysHead{Length: n, Head: head}

	_, err = k.get(p, "PUT", "/keys", entries)
	if err == nil {
		var got connector.KeysHead
		if got, err = k.keys(p); err == nil && got != want {
			err = fmt.Errorf("GET /keys answers %+v after PUT, not %+v", got, want)
		}
	}
	k.check("PUT /keys takes the first log", err)

	status, body, err := k.call(p, "PUT", "/keys", []record.Envelope{e0, keyEntry(1, "add", c, &e0, a)})
	if err == nil && status != http.StatusConflict {
		err = fmt.Errorf("answered %d, not 409: %s", status, bytes.TrimSpace(body))
	}
	if err == nil {
		var got connector.KeysHead
		if got, err = k.keys(p); err == nil && got != want {
			err = fmt.Errorf("GET /keys answers %+v after the refusal, not %+v", got, want)
		}
	}
	k.check("PUT /keys refuses a rewritten log", err)

	op, args, display, prepared := k.baseline(p, spec, data)
	now := time.Now()
	write := func(change func(*record.Write)) record.Write {
		w := record.Write{
			ID: record.NewID(), Connector: name, Op: op, Args: args, Display: display,
			CreatedAt: record.Time{Time: now}, ExpiresAt: record.Time{Time: now.Add(time.Hour)},
		}
		if change != nil {
			change(&w)
		}
		return w
	}
	execute := func(e record.Envelope) error {
		return refused(k.call(p, "POST", "/execute", e))
	}

	bad := sign(write(nil), now, a)
	bad.Sig[len(bad.Sig)-1] ^= 1
	k.check("execute refuses a bad signature", execute(bad))
	k.check("execute refuses an untrusted key", execute(sign(write(nil), now, c)))
	k.check("execute refuses a removed key", execute(sign(write(nil), now, b)))
	k.check("execute refuses a retired key", execute(sign(write(nil), now, d)))
	k.check("execute refuses an expired record", execute(sign(write(func(w *record.Write) {
		w.CreatedAt = record.Time{Time: now.Add(-2 * time.Hour)}
		w.ExpiresAt = record.Time{Time: now.Add(-time.Hour)}
	}), now, a)))
	k.check("execute refuses a record issued over 5 minutes ago", execute(sign(write(nil), now.Add(-10*time.Minute), a)))
	k.check("execute refuses another connector's record", execute(sign(write(func(w *record.Write) {
		w.Connector = name + "-other"
	}), now, a)))

	changed := sign(write(func(w *record.Write) {
		w.Display = append(slices.Clone(w.Display), record.Field{Label: "Note", Value: "added after prepare"})
	}), now, a)
	if prepared {
		status, body, err := k.call(p, "POST", "/execute", changed)
		if err == nil && status != http.StatusConflict {
			err = fmt.Errorf("answered %d, not 409: %s", status, bytes.TrimSpace(body))
		}
		k.check("execute refuses a changed display", err)
	} else {
		err := execute(changed)
		if err == nil {
			err = skip("refused, but no write op prepares without an account, so the refusal may be prepare's")
		}
		k.check("execute refuses a changed display", err)
	}
}

// baseline is a write as prepare renders it, if any write op prepares
// without an account, with a stored chat where the op takes one; else
// made-up args and display.
func (k *kit) baseline(p *proc, spec connector.Spec, data string) (op string, args map[string]string, display []record.Field, prepared bool) {
	chat, seq := sampleChat(k.ctx, data)
	var writes []connector.Op
	for _, o := range spec.Ops {
		if o.Kind == connector.Write {
			writes = append(writes, o)
		}
	}
	for _, o := range writes {
		args := make(map[string]string)
		for _, p := range o.Params {
			switch {
			case p.Container:
				args[p.Name] = chat
			case p.Message && p.Required:
				args[p.Name] = seq
			case p.Required:
				args[p.Name] = "conformance"
			}
		}
		b, err := k.get(p, "POST", "/prepare", map[string]any{"op": o.Name, "args": args})
		var res connector.Prepared
		if err == nil && json.Unmarshal(b, &res) == nil && res.Args != nil {
			return o.Name, res.Args, res.Display, true
		}
	}
	op = "send"
	if len(writes) > 0 {
		op = writes[0].Name
	}
	return op, map[string]string{"text": "conformance"}, []record.Field{{Label: "Text", Value: "conformance"}}, false
}

// sampleChat is a stored chat and a message seq in it, or made-up ones.
func sampleChat(ctx context.Context, data string) (chat, seq string) {
	chat, seq = "conformance", "1"
	db, err := openStore(data)
	if err != nil {
		return chat, seq
	}
	defer db.Close()
	if db.QueryRowContext(ctx, `SELECT id FROM containers ORDER BY id LIMIT 1`).Scan(&chat) == nil {
		_ = db.QueryRowContext(ctx, `SELECT seq FROM messages WHERE container_id = ?1 LIMIT 1`, chat).Scan(&seq)
	}
	return chat, seq
}
