package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/adeci/mandate/connector"
)

type connectorInfo struct {
	connector.Spec
	Health *connector.Health `json:"health"`
	Source bool              `json:"source"`
}

// Help prints the agent commands, with the connectors the gateway serves when
// it can reach one.
func Help(ctx context.Context, w io.Writer) {
	fmt.Fprint(w, `mandate reads the owner's accounts. Anything that writes is a proposal the
owner approves first.

usage:
  mandate <connector> <command> [args]
  mandate status ID [--wait DUR]

`)
	c, err := fromEnv(os.Getenv, w, w, false)
	var list []connectorInfo
	if err == nil {
		err = c.connectors(ctx, &list)
	}
	if err != nil {
		fmt.Fprintf(w, "connectors: unavailable (%v)\n", err)
	} else {
		fmt.Fprintln(w, "connectors:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, ci := range list {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", ci.Name, health(ci.Health), strings.Join(commands(ci), " "))
		}
		_ = tw.Flush()
	}
	fmt.Fprint(w, `
Run mandate <connector> --help for its commands.

Output is TSV with a header line (a table on a terminal), or JSON with --json.
TSV escapes backslash, tab, newline and CR in values as \\, \t, \n and \r, and
other control characters as \xHH or \uHHHH. Times are UTC.
After proposing, tell the owner it is waiting, then run mandate status ID --wait 15m.

exit codes:
  0   done
  75  not yet: pending, rate limited, or --wait ran out
  1   rejected, expired, failed, or unknown (it may have been sent; don't resend)
  2   usage
`)
}

func (c *client) connectors(ctx context.Context, list *[]connectorInfo) error {
	_, _, raw, err := c.do(ctx, requestTimeout, http.MethodGet, "/v1/connectors", nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, list)
}

func commands(ci connectorInfo) []string {
	var out []string
	if ci.Source {
		out = append(out, "chats", "messages", "search")
		if ci.Media {
			out = append(out, "media")
		}
	}
	for _, op := range ci.Ops {
		out = append(out, op.Name)
	}
	if ci.Forward {
		out = append(out, "get")
	}
	return out
}

func (c *client) connectorHelp(ctx context.Context, name string) error {
	var list []connectorInfo
	if err := c.connectors(ctx, &list); err != nil {
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
	return usage("mandate <connector> <command> [args]", fmt.Errorf("no connector %q; connectors: %s", name, strings.Join(names, ", ")))
}

func writeConnectorHelp(w io.Writer, ci connectorInfo) {
	fmt.Fprintf(w, "%s: %s\n", ci.Name, health(ci.Health))
	if doc := strings.TrimSpace(ci.Doc); doc != "" {
		fmt.Fprintf(w, "\n%s\n", doc)
	}
	fmt.Fprintln(w)
	if ci.Source {
		fmt.Fprintf(w, "  "+chatsUsage+"\n      id kind name last_message, most recent first\n", ci.Name)
		fmt.Fprintf(w, "  "+messagesUsage+"\n      seq sent_at sender_id sender reply_to body, newest first\n", ci.Name)
		fmt.Fprintf(w, "  "+searchUsage+"\n      seq sent_at chat_id chat sender_id sender reply_to body, newest first\n", ci.Name)
		if ci.Media {
			fmt.Fprintf(w, "  "+mediaUsage+"\n      save a message's file, by default as %s-SEQ.EXT here; prints path type size name\n", ci.Name, ci.Name)
		}
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
			fmt.Fprint(w, " [--note WHY] [--wait DUR]")
		}
		fmt.Fprintf(w, "\n      %s (%s)\n", op.Summary, kind)
		for _, p := range op.Params {
			fmt.Fprintf(w, "      %s: %s%s\n", p.Name, p.Doc, paramTags(p))
		}
	}
	if ci.Forward {
		fmt.Fprintf(w, "  "+getUsage+"\n      raw read; PATH may carry a ?query\n", ci.Name)
	}
	fmt.Fprintln(w)
	if ci.Source {
		fmt.Fprintln(w, "Chats are addressed by id: find one with chats --search, then pass its id.")
		fmt.Fprintln(w, "sender me is the owner; reply_to is the seq replied to. Bodies may carry [image/jpeg]")
		fmt.Fprintln(w, "(or another media type), [deleted] or [login code hidden].")
		if ci.Media {
			fmt.Fprintln(w, "A message with several files marks them [#0 image/png] [#1 ...]; pass that --index.")
		}
		fmt.Fprintln(w, "When more rows exist, stderr gives the --before value for the next page.")
		if reads {
			fmt.Fprintln(w, "Read ops print messages in search's columns.")
		}
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
	if p.Editable {
		tags = append(tags, "owner may edit")
	}
	if len(tags) == 0 {
		return ""
	}
	return " (" + strings.Join(tags, ", ") + ")"
}

func health(h *connector.Health) string {
	if h == nil {
		return "stored messages only"
	}
	s := string(h.State)
	switch h.State {
	case connector.Healthy:
		return s
	case connector.Resting:
		if !h.Until.IsZero() {
			s += " until " + h.Until.UTC().Format(time.RFC3339)
		}
	default:
		s = "needs the owner (" + s + ")"
	}
	if h.Detail != "" {
		s += ": " + h.Detail
	}
	return s
}
