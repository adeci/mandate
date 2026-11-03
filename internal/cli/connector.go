package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/adeci/mandate/connector"
)

const (
	chatsUsage    = "mandate %s chats [--search TEXT] [--limit 20]"
	messagesUsage = "mandate %s messages CHAT_ID [--limit 20] [--before SEQ] [--seq SEQ] [--full]"
	searchUsage   = "mandate %s search TEXT [--chat CHAT_ID] [--since YYYY-MM-DD] [--limit 50] [--before SEQ] [--full]"
	mediaUsage    = "mandate %s media CHAT_ID SEQ [--index N] [-o PATH]"
	sendUsage     = "mandate %s send CHAT_ID TEXT [--note WHY] [--wait 15m]"
	opUsage       = "mandate %s %s [KEY=VALUE...] [--note WHY] [--wait 15m]"
)

type connectorInfo struct {
	Name   string           `json:"name"`
	Doc    string           `json:"doc"`
	Media  bool             `json:"media"`
	Ops    []connector.Op   `json:"ops"`
	Health connector.Health `json:"health"`
	Access window           `json:"access"`
}

// connector runs mandate NAME ARGS.
func (c *cli) connector(ctx context.Context, name string, args []string) error {
	c.name = name
	if len(args) == 0 || isHelp(args[0]) {
		return c.connectorHelp(ctx, name)
	}
	cmd, args := args[0], args[1:]
	base := "/agent/v1/" + url.PathEscape(name) + "/"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "chats":
		help := fmt.Sprintf(chatsUsage, name)
		search := fs.String("search", "", "")
		limit := fs.Int("limit", 0, "")
		if _, err := c.parse(fs, help, args, 0, 0); err != nil {
			return err
		}
		return c.rows(ctx, base+"chats"+query("search", *search, "limit", *limit))
	case "messages":
		help := fmt.Sprintf(messagesUsage, name)
		limit := fs.Int("limit", 0, "")
		before := fs.Int("before", 0, "")
		seq := fs.Int("seq", 0, "")
		fs.BoolVar(&c.full, "full", false, "")
		pos, err := c.parse(fs, help, args, 1, 1)
		if err != nil {
			return err
		}
		if *seq > 0 {
			c.seq, c.full = int64(*seq), true
			*before, *limit = *seq+1, 1
		}
		return c.rows(ctx, base+"messages"+query("chat", pos[0], "limit", *limit, "before", *before))
	case "search":
		help := fmt.Sprintf(searchUsage, name)
		chat := fs.String("chat", "", "")
		since := fs.String("since", "", "")
		limit := fs.Int("limit", 0, "")
		before := fs.Int("before", 0, "")
		fs.BoolVar(&c.full, "full", false, "")
		pos, err := c.parse(fs, help, args, 1, -1)
		if err != nil {
			return err
		}
		return c.rows(ctx, base+"search"+query("text", strings.Join(pos, " "), "chat", *chat, "since", *since, "limit", *limit, "before", *before))
	case "media":
		return c.media(ctx, name, args)
	}
	help := fmt.Sprintf(opUsage, name, cmd)
	if cmd == "send" {
		help = fmt.Sprintf(sendUsage, name)
	}
	note := fs.String("note", "", "")
	wait := fs.Duration("wait", defaultWait, "")
	pos, err := c.parse(fs, help, args, 0, -1)
	if err != nil {
		return err
	}
	if *wait < 0 {
		return usage(help, errNegativeWait)
	}
	// send CHAT_ID TEXT is shorthand for chat= and text=; a send whose
	// params differ, like an email's, takes KEY=VALUE as other ops do.
	if cmd == "send" && (len(pos) == 0 || !isKeyValue(pos[0])) {
		if len(pos) < 2 {
			return usage(help, nil)
		}
		return c.submit(ctx, base+"send", map[string]string{"chat": pos[0], "text": strings.Join(pos[1:], " ")}, *note, *wait)
	}
	kv, err := keyValues(pos)
	if err != nil {
		return usage(fmt.Sprintf(opUsage, name, cmd), err)
	}
	return c.submit(ctx, base+url.PathEscape(cmd), kv, *note, *wait)
}

// isKeyValue reports whether s starts with a param name and "=". Base64
// chat IDs end in "=" too, but hold more than lowercase letters and digits.
func isKeyValue(s string) bool {
	k, _, ok := strings.Cut(s, "=")
	if !ok || k == "" || k[0] < 'a' || k[0] > 'z' {
		return false
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func keyValues(args []string) (map[string]string, error) {
	kv := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("want KEY=VALUE, got %q", a)
		}
		if _, dup := kv[k]; dup {
			return nil, fmt.Errorf("%s given twice", k)
		}
		kv[k] = v
	}
	return kv, nil
}

func (c *cli) rows(ctx context.Context, path string) error {
	raw, _, err := c.do(ctx, requestTimeout, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	return c.table(raw)
}

// media saves a message's file and prints where it went.
func (c *cli) media(ctx context.Context, name string, args []string) error {
	help := fmt.Sprintf(mediaUsage, name)
	fs := flag.NewFlagSet("media", flag.ContinueOnError)
	index := fs.Int("index", 0, "")
	out := fs.String("o", "", "")
	pos, err := c.parse(fs, help, args, 2, 2)
	if err != nil {
		return err
	}
	seq, err := strconv.Atoi(pos[1])
	if err != nil || seq < 1 {
		return usage(help, fmt.Errorf("SEQ is a message's seq from mandate %s messages, not %q", name, pos[1]))
	}
	if *index < 0 {
		return usage(help, errors.New("--index must not be negative"))
	}
	ctx, cancel := context.WithTimeout(ctx, mediaTimeout)
	defer cancel()
	resp, err := c.send(ctx, http.MethodGet, "/agent/v1/"+url.PathEscape(name)+"/media"+query("chat", pos[0], "seq", seq, "index", *index), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	saved := struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size"`
		Name string `json:"name,omitempty"`
	}{Path: *out, Type: resp.Header.Get("Content-Type")}
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		saved.Name = params["filename"]
	}
	if saved.Path == "" {
		saved.Path = mediaName(name, seq, *index, saved.Type, saved.Name)
	}
	f, err := os.Create(saved.Path)
	if err != nil {
		return err
	}
	saved.Size, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(saved.Path)
		return fmt.Errorf("save %s: %w", saved.Path, err)
	}
	if c.json {
		return json.NewEncoder(c.out).Encode(saved)
	}
	return c.printTable(table{Columns: []string{"path", "type", "size", "name"}, Rows: [][]any{{saved.Path, saved.Type, saved.Size, saved.Name}}})
}

// mediaName names a file after the connector, message and index, so fetching
// it again replaces it. Of the sender's file name only the extension is used.
func mediaName(connector string, seq, index int, typ, name string) string {
	base := fmt.Sprintf("%s-%d", connector, seq)
	if index > 0 {
		base += fmt.Sprintf("-%d", index)
	}
	return base + extension(typ, name)
}

var plainExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`)

// Extensions for the types chats carry most, where mime's answer depends on
// the system's mime.types or lists several.
var extensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"video/webm":      ".webm",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"audio/aac":       ".aac",
	"audio/amr":       ".amr",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

func extension(typ, name string) string {
	if ext := filepath.Ext(name); plainExt.MatchString(ext) {
		return strings.ToLower(ext)
	}
	mt, _, err := mime.ParseMediaType(typ)
	if err != nil {
		return ""
	}
	if ext, ok := extensions[mt]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

func (c *cli) connectors(ctx context.Context) ([]connectorInfo, []byte, error) {
	var list []connectorInfo
	raw, _, err := c.do(ctx, requestTimeout, http.MethodGet, "/agent/v1/connectors", nil, &list)
	return list, raw, err
}

// listConnectors prints each connector's state, read access and commands.
func (c *cli) listConnectors(ctx context.Context, args []string) error {
	if _, err := c.parse(flag.NewFlagSet("connectors", flag.ContinueOnError), "mandate connectors [--json]", args, 0, 0); err != nil {
		return err
	}
	list, raw, err := c.connectors(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(raw)
	}
	t := table{Columns: []string{"name", "health", "access", "commands"}}
	for _, ci := range list {
		t.Rows = append(t.Rows, []any{ci.Name, health(ci.Health), ci.Access.String(), strings.Join(commands(ci), " ")})
	}
	if err := c.printTable(t); err != nil {
		return err
	}
	fmt.Fprintln(c.errOut, "mandate CONNECTOR --help shows a connector's commands")
	return nil
}

func commands(ci connectorInfo) []string {
	out := []string{"chats", "messages", "search"}
	if ci.Media {
		out = append(out, "media")
	}
	for _, op := range ci.Ops {
		out = append(out, op.Name)
	}
	return out
}

func (c *cli) connectorHelp(ctx context.Context, name string) error {
	list, _, err := c.connectors(ctx)
	if err != nil {
		return err
	}
	var names []string
	for _, ci := range list {
		if ci.Name == name {
			writeConnectorHelp(c.out, ci)
			return nil
		}
		names = append(names, ci.Name)
	}
	return usage("mandate CONNECTOR COMMAND [ARGS]", fmt.Errorf("no command or connector %q; connectors: %s", name, strings.Join(names, ", ")))
}

func writeConnectorHelp(w io.Writer, ci connectorInfo) {
	fmt.Fprintf(w, "%s: %s; read access %s\n", ci.Name, health(ci.Health), ci.Access)
	if doc := strings.TrimSpace(ci.Doc); doc != "" {
		fmt.Fprintf(w, "\n%s\n", sanitize(doc))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  "+chatsUsage+"\n      id kind name last_message, most recent first\n", ci.Name)
	fmt.Fprintf(w, "  "+messagesUsage+"\n      seq sent_at sender_id sender reply_to body, newest first\n", ci.Name)
	fmt.Fprintf(w, "  "+searchUsage+"\n      seq sent_at chat_id chat sender_id sender reply_to body, newest first\n", ci.Name)
	if ci.Media {
		fmt.Fprintf(w, "  "+mediaUsage+"\n      save a message's file, by default as %s-SEQ.EXT here; prints path type size name\n", ci.Name, ci.Name)
	}
	writes, reads := false, false
	for _, op := range ci.Ops {
		kind := "read"
		if op.Kind == connector.Write {
			kind, writes = "proposal", true
		} else {
			reads = true
		}
		if op.Name == "send" && chatText(op.Params) {
			fmt.Fprintf(w, "  "+sendUsage+"\n      %s (%s)\n", ci.Name, op.Summary, kind)
			continue
		}
		fmt.Fprintf(w, "  mandate %s %s", ci.Name, op.Name)
		for _, p := range op.Params {
			if p.Required {
				fmt.Fprintf(w, " %s=...", p.Name)
			} else {
				fmt.Fprintf(w, " [%s=...]", p.Name)
			}
		}
		if op.Kind == connector.Write {
			fmt.Fprint(w, " [--note WHY] [--wait 15m]")
		}
		fmt.Fprintf(w, "\n      %s (%s)\n", op.Summary, kind)
		for _, p := range op.Params {
			fmt.Fprintf(w, "      %s: %s%s\n", p.Name, p.Doc, paramTags(p))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Chats are addressed by id: find one with chats --search, then pass its id.")
	fmt.Fprintln(w, "sender me is the owner; reply_to is the seq replied to. Bodies may carry [image/jpeg]")
	fmt.Fprintln(w, "(or another media type), [deleted] or [login code hidden].")
	if ci.Media {
		fmt.Fprintln(w, "A message with several files marks them [#0 image/png] [#1 ...]; pass that --index.")
	}
	fmt.Fprintln(w, "When more rows exist, stderr gives the --before value for the next page.")
	fmt.Fprintf(w, "Bodies over %d characters are cut; --full prints them whole, --seq SEQ one message whole.\n", bodyPreview)
	if reads {
		fmt.Fprintln(w, "Read ops print messages in search's columns.")
	}
	if writes {
		fmt.Fprintln(w, "--note is shown to the owner with the proposal.")
	}
	fmt.Fprintln(w, "Every command takes --json.")
}

// chatText reports whether a send op takes the CHAT_ID TEXT shorthand.
func chatText(params []connector.Param) bool {
	return len(params) == 2 && params[0].Name == "chat" && params[1].Name == "text"
}

func paramTags(p connector.Param) string {
	var tags []string
	if p.Container {
		tags = append(tags, "a chat ID")
	}
	if p.Message {
		tags = append(tags, "a message's seq")
	}
	if p.Editable {
		tags = append(tags, "owner may edit")
	}
	if len(tags) == 0 {
		return ""
	}
	return " (" + strings.Join(tags, ", ") + ")"
}

func health(h connector.Health) string {
	s := string(h.State)
	switch h.State {
	case connector.Healthy, connector.Connecting:
	case connector.Resting:
		if !h.Until.IsZero() {
			s += " until " + h.Until.UTC().Format(timeFormat)
		}
	default:
		s = "needs the owner (" + s + ")"
	}
	if h.Detail != "" {
		s += ": " + sanitize(h.Detail)
	}
	return s
}

// help prints the agent commands, with the connectors when logged in.
func (c *cli) help(ctx context.Context) error {
	fmt.Fprint(c.out, `mandate reads the owner's accounts. Reads need access the owner grants;
anything that writes is a proposal the owner approves first.

usage:
  `+loginUsage+`
  mandate connectors
  mandate CONNECTOR COMMAND [ARGS]
  mandate access
  `+requestUsages+`
  mandate status ID [--wait 15m]

`)
	var list []connectorInfo
	err := c.connect()
	if err == nil {
		list, _, err = c.connectors(ctx)
	}
	if err != nil {
		fmt.Fprintf(c.out, "connectors: unavailable (%v)\n", err)
	} else {
		fmt.Fprintln(c.out, "connectors:")
		tw := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		for _, ci := range list {
			fmt.Fprintf(tw, "  %s\t%s\tread %s\t%s\n", ci.Name, health(ci.Health), ci.Access, strings.Join(commands(ci), " "))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprint(c.out, `
Run mandate CONNECTOR --help for its commands.

Output is TSV with a header line (a table on a terminal), or JSON with --json.
TSV escapes backslash, tab, newline and CR in values as \\, \t, \n and \r, and
other control characters as \xHH or \uHHHH. Times are UTC.

Reading a connector needs an open grant: mandate access lists yours. So does
proposing a write, which may only target or answer chats and messages the
grant lets you read; writes still work for a day after a window closes. If
the task needs access, ask once with mandate request: every chat the task
needs in that one request and no more (--chat matches chat names), the
shortest --for, a --since that covers only what the task needs, and a
--reason the owner can judge. The owner decides in the mandate app. Extend a
window with --extend rather than asking again; for recurring work, request a
routine with --every and --at. Don't request in a loop.

request, a proposal and status wait for the owner's decision, 15m unless
--wait says otherwise (0 returns at once).

exit codes:
  0   done, or access granted
  75  not yet: pending, rate limited, or --wait ran out
  1   rejected, denied, expired, failed, or unknown (it may have been sent; don't resend)
  2   usage
`)
	return nil
}
