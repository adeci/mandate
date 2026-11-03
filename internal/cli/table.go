package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
)

// bodyPreview is where listings cut a body, so an email thread doesn't fill
// an agent's context; --full and --seq print bodies whole.
const bodyPreview = 600

type table struct {
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	More       bool     `json:"more,omitempty"`
	NextBefore int64    `json:"next_before,omitempty"`
	Containers int      `json:"containers,omitempty"`
}

// table prints a read's rows as TSV, or aligned on a terminal; the footer
// goes to stderr so stdout stays parseable.
func (c *cli) table(raw []byte) error {
	var t table
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	seqCol := slices.Index(t.Columns, "seq")
	if c.seq > 0 && (len(t.Rows) == 0 || seqCol < 0 || cell(t.Rows[0][seqCol]) != strconv.FormatInt(c.seq, 10)) {
		return fmt.Errorf("no message %d in that chat", c.seq)
	}
	if c.json {
		return c.printJSON(raw)
	}
	if err := c.printTable(t); err != nil {
		return err
	}
	switch {
	case c.seq > 0:
	case t.NextBefore > 0:
		fmt.Fprintf(c.errOut, "next: --before %d\n", t.NextBefore)
	case t.More:
		fmt.Fprintln(c.errOut, "more: raise --limit (or offset= for an op), or narrow the search")
	}
	if t.Containers > 0 {
		fmt.Fprintf(c.errOut, "%d chats updated; list them with mandate %s chats\n", t.Containers, c.name)
	}
	return nil
}

func (c *cli) printTable(t table) error {
	w := c.out
	var tw *tabwriter.Writer
	if c.tty {
		tw = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		w = tw
	}
	body := slices.Index(t.Columns, "body")
	// A sync answers only a count of chats.
	if len(t.Rows) > 0 || t.Containers == 0 {
		fmt.Fprintln(w, strings.Join(t.Columns, "\t"))
	}
	for _, row := range t.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if s, ok := v.(string); ok && i == body && !c.full {
				v = preview(s)
			}
			cells[i] = cell(v)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	if tw != nil {
		return tw.Flush()
	}
	return nil
}

// preview cuts a long body and says how to read the rest.
func preview(s string) string {
	r := []rune(s)
	if len(r) <= bodyPreview {
		return s
	}
	return fmt.Sprintf("%s…[+%d chars; messages CHAT_ID --seq SEQ prints it whole]", string(r[:bodyPreview]), len(r)-bodyPreview)
}

// escapeTSV keeps a value on one line and in one cell; help documents it.
var escapeTSV = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// sanitize writes control characters other than tab and newline as \xHH
// or \uHHHH, so a message can't send escape sequences to the terminal.
func sanitize(s string) string {
	if !strings.ContainsFunc(s, control) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !control(r):
			b.WriteRune(r)
		case r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// control is true for C0 and C1 controls and DEL, except tab and newline.
func control(r rune) bool {
	return unicode.IsControl(r) && r != '\t' && r != '\n'
}

func cell(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return sanitize(escapeTSV.Replace(fmt.Sprint(v)))
	}
}
