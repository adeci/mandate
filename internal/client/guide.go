package client

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/adeci/mandate/connector"
)

func (c *client) guide(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usage(guideUsage, nil)
	}
	var s schemaInfo
	if err := c.get(ctx, "/v1/schema", &s); err != nil {
		return err
	}
	writeGuide(c.out, s)
	return nil
}

// writeGuide renders markdown short enough for a system prompt, from the
// gateway's live schema so it cannot drift from what the gateway serves.
func writeGuide(w io.Writer, s schemaInfo) {
	fmt.Fprint(w, `# mandate

mandate lets you read the owner's chats with SQL and call some APIs through the `+"`mandate`"+` CLI, without any credentials.
Writes (messages, API changes) are proposals: they run only after the owner approves them with a passkey.
Output is JSON when piped: rows come as {"columns":[...],"rows":[[...]]}.

## Rules

- Never ask the owner for passwords, tokens or codes; mandate holds every credential.
- Every write needs the owner's approval. Propose it, then send the owner the proposal's url in this chat.
- Don't retry a rejected proposal unless the owner asks. An unknown result may have been sent: never retry it.
- Login codes and links are withheld (body NULL, is_auth = 1); don't try to get them another way.

`)

	if len(s.Views) > 0 {
		fmt.Fprintf(w, "## Reading: mandate query SQL [PARAM...]\n\n")
		fmt.Fprintf(w, "One read-only SELECT over these views; PARAMs bind to ?1, ?2, ...")
		if s.Limits.MaxRows > 0 {
			fmt.Fprintf(w, " At most %d rows.", s.Limits.MaxRows)
		}
		fmt.Fprint(w, "\n\n")
		for _, v := range s.Views {
			fmt.Fprintf(w, "- `%s`: %s\n  Columns: %s\n", v.Name, v.Description, strings.Join(v.Columns, ", "))
		}
		writeRecipes(w, s)
	}

	if len(s.Connectors) > 0 {
		fmt.Fprint(w, "\n## Connectors\n\nRead ops run at once and answer with rows like <connector>_messages; write ops are proposals.\nContainer params name a chat or channel; IDs from <connector>_containers are exact.\n")
	}
	for _, spec := range s.Connectors {
		fmt.Fprintf(w, "\n### %s (%s)\n\n", spec.Name, health(spec.Health))
		switch spec.Health.State {
		case connector.Healthy, "":
		case connector.Resting:
			fmt.Fprint(w, "Its reads and proposals are refused until then; retry after.\n\n")
		default:
			fmt.Fprint(w, "Its reads and proposals fail until the owner fixes it; tell them if you need it.\n\n")
		}
		if doc := strings.TrimSpace(spec.Doc); doc != "" {
			fmt.Fprintf(w, "%s\n\n", doc)
		}
		for _, op := range spec.Ops {
			fmt.Fprintf(w, "- `%s`: %s\n", opUsage(spec.Name, op), op.Summary)
			for _, p := range op.Params {
				fmt.Fprintf(w, "  - %s: %s%s\n", p.Name, p.Doc, paramTags(p))
			}
		}
		if spec.Name == "http" {
			fmt.Fprint(w, "\nShortcut: `mandate http GET SERVICE PATH` reads directly; `mandate http POST SERVICE PATH --data BODY` (or PUT, PATCH, DELETE) proposes `http request`.\n")
		}
	}

	fmt.Fprint(w, `
## Writing

1. `+"`mandate propose CONNECTOR OP KEY=VALUE... --note \"why\"`"+` prints the proposal with its id and url.
2. Send the owner the url in this chat, with one line on what it does.
3. `+"`mandate status ID --wait 15m`"+` blocks until they decide (or pass --wait to propose).

The owner may edit some fields before approving; the result shows what ran.
Statuses: pending, executing, then done, failed or unknown; or rejected, expired.

## Exit codes

- 0: done, or the read worked.
- 75: still pending, paced, or --wait ran out; stderr says when to retry.
- 1: rejected, expired, failed, unknown, or an error; stderr says what to do next.
- 2: wrong usage.
`)
}

// writeRecipes shows the common queries against the first source with messages.
func writeRecipes(w io.Writer, s schemaInfo) {
	var source string
	for _, v := range s.Views {
		if name, ok := strings.CutSuffix(v.Name, "_messages"); ok {
			source = name
			break
		}
	}
	if source == "" {
		return
	}
	msgs, chats := source+"_messages", source+"_containers"
	fmt.Fprintf(w, "\nRecipes (names are partial, so match with LIKE; every source's views have the same columns):\n\n```sh\n")
	for _, r := range [][2]string{
		{"a person's or chat's latest messages", fmt.Sprintf(`mandate query "SELECT sent_at, container_name, sender_name, body FROM %s WHERE container_name LIKE ?1 OR sender_name LIKE ?1 ORDER BY sent_at DESC LIMIT 20" %%Farid%%`, msgs)},
		{"search text", fmt.Sprintf(`mandate query "SELECT sent_at, container_name, sender_name, body FROM %s WHERE body LIKE ?1 ORDER BY sent_at DESC LIMIT 50" %%drill%%`, msgs)},
		{"new since a cursor (keep the highest seq you saw)", fmt.Sprintf(`mandate query "SELECT seq, sent_at, container_name, sender_name, body FROM %s WHERE seq > ?1 ORDER BY seq LIMIT 200" 0`, msgs)},
		{"list chats, most recent first", fmt.Sprintf(`mandate query "SELECT id, kind, name, updated_at FROM %s ORDER BY updated_at DESC LIMIT 50"`, chats)},
		{"busiest chats this week", fmt.Sprintf(`mandate query "SELECT container_name, count(*) AS n FROM %s WHERE sent_at >= date('now', '-7 days') GROUP BY container_id ORDER BY n DESC LIMIT 10"`, msgs)},
	} {
		fmt.Fprintf(w, "# %s\n%s\n", r[0], r[1])
	}
	fmt.Fprint(w, "```\n")
}
