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
	fmt.Fprint(w, `usage: mandate <connector> <command> [args]   (needs MANDATE_URL and MANDATE_TOKEN)
       mandate status ID [--wait DUR]

Reads run at once. Writes become proposals: tell the owner one is waiting;
they approve it in the console.

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
mandate <connector> --help lists its commands.
Output: TSV with a header when piped, a table on a terminal, --json for JSON.
Exit: 0 done, 75 pending, paced or timed out, 1 rejected, expired, failed, unknown or error, 2 usage.
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
		fmt.Fprintf(w, "  "+chatsUsage+"\n      id kind name updated_at\n", ci.Name)
		fmt.Fprintf(w, "  "+messagesUsage+"\n      seq sent_at sender body, newest first\n", ci.Name)
		fmt.Fprintf(w, "  "+searchUsage+"\n      seq sent_at chat sender body, newest first\n", ci.Name)
	}
	for _, op := range ci.Ops {
		kind := "read"
		if op.Kind == connector.Write {
			kind = "proposal"
		}
		if op.Name == "send" {
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
	fmt.Fprintln(w, "\nEvery command takes --json.")
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
		return "stored chats only"
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
