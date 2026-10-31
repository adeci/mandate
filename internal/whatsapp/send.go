package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/store"
)

// sendClient is the part of *whatsmeow.Client that sends.
type sendClient interface {
	IsConnected() bool
	IsLoggedIn() bool
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// waConnector sends approved text messages. Recipients are resolved from
// local state only: the address book and LID map in the session database and
// the groups in data.db.
type waConnector struct {
	cli sendClient
	dev *wastore.Device
	st  *store.Store
	log *slog.Logger
}

func newConnector(cli sendClient, dev *wastore.Device, st *store.Store, log *slog.Logger) *waConnector {
	return &waConnector{cli: cli, dev: dev, st: st, log: log}
}

func (c *waConnector) Spec() connector.Spec {
	return connector.Spec{Name: "whatsapp", Ops: []connector.Op{{
		Name:    "send",
		Kind:    connector.Write,
		Summary: "Send a text message to a contact or group.",
		Params: []connector.Param{
			{
				Name:      "to",
				Doc:       "chat JID (…@s.whatsapp.net or …@g.us), phone number with country code, or exact address-book contact or group name",
				Required:  true,
				Container: true,
			},
			{Name: "text", Doc: "message text", Required: true, Editable: true},
		},
	}}}
}

type recipient struct {
	jid   types.JID
	name  string // address-book or group name; empty for numbers not in the address book
	group bool
}

func (r recipient) field() approval.Field {
	switch {
	case r.group:
		return approval.Field{Label: "Group", Value: r.name}
	case r.name != "":
		return approval.Field{Label: "To", Value: r.name + " (" + formatPhone(r.jid.User, false) + ")"}
	default:
		return approval.Field{Label: "To", Value: formatPhone(r.jid.User, false) + " (not in your contacts)"}
	}
}

func (c *waConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
	if op != "send" {
		return connector.Prepared{}, connector.ErrUnknownOp
	}
	for k := range args {
		if k != "to" && k != "text" {
			return connector.Prepared{}, connector.Invalid("unknown arg %q", k)
		}
	}
	text := args["text"]
	if strings.TrimSpace(text) == "" {
		return connector.Prepared{}, connector.Invalid("text is required")
	}
	r, err := c.resolve(ctx, args["to"])
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"to": r.jid.String(), "text": text},
		Display:   []approval.Field{r.field(), {Label: "Text", Value: text, Param: "text"}},
		Container: r.jid.String(),
	}, nil
}

// Phone numbers are not checked with IsOnWhatsApp: that is a network query
// WhatsApp rate-limits and watches for number scraping, and Prepare runs for
// every proposal and again at execution. Sending to a number without WhatsApp
// fails upstream instead.
func (c *waConnector) resolve(ctx context.Context, to string) (recipient, error) {
	to = strings.TrimSpace(to)
	switch {
	case to == "":
		return recipient{}, connector.Invalid("to is required")
	case strings.Contains(to, "@"):
		j, err := types.ParseJID(to)
		if err != nil {
			return recipient{}, connector.Invalid("invalid JID %q", to)
		}
		return c.byJID(ctx, j.ToNonAD())
	}
	if digits, ok := phoneDigits(to); ok {
		return c.byJID(ctx, types.NewJID(digits, types.DefaultUserServer))
	}
	return c.byName(ctx, to)
}

func (c *waConnector) byJID(ctx context.Context, j types.JID) (recipient, error) {
	switch j.Server {
	case types.DefaultUserServer:
		if d, ok := phoneDigits(j.User); !ok || d != j.User {
			return recipient{}, connector.Invalid("invalid user JID %q", j.String())
		}
		name, err := c.addressBookName(ctx, j)
		if err != nil {
			return recipient{}, err
		}
		return recipient{jid: j, name: name}, nil
	case types.GroupServer:
		groups, err := c.groups(ctx)
		if err != nil {
			return recipient{}, err
		}
		for _, g := range groups {
			if g.jid == j {
				return g, nil
			}
		}
		return recipient{}, connector.Invalid("unknown group %s", j.String())
	}
	return recipient{}, connector.Invalid("cannot send to %s: use a phone number, a name, or a …@s.whatsapp.net or …@g.us JID", j.String())
}

// Push names are chosen by the sender and never identify a recipient.
func addressBookName(ci types.ContactInfo) string {
	if ci.FullName != "" {
		return ci.FullName
	}
	return ci.FirstName
}

// Address-book entries may be keyed by phone number or by LID.
func (c *waConnector) addressBookName(ctx context.Context, pn types.JID) (string, error) {
	ci, err := c.dev.Contacts.GetContact(ctx, pn)
	if err != nil {
		return "", fmt.Errorf("look up contact: %w", err)
	}
	if n := addressBookName(ci); n != "" {
		return n, nil
	}
	lid, err := c.dev.LIDs.GetLIDForPN(ctx, pn)
	if err != nil {
		return "", fmt.Errorf("look up lid: %w", err)
	}
	if lid.IsEmpty() {
		return "", nil
	}
	ci, err = c.dev.Contacts.GetContact(ctx, lid)
	if err != nil {
		return "", fmt.Errorf("look up contact: %w", err)
	}
	return addressBookName(ci), nil
}

func (c *waConnector) groups(ctx context.Context) ([]recipient, error) {
	rows, err := c.st.DB().QueryContext(ctx,
		`SELECT id, COALESCE(name, '') FROM containers WHERE is_group = 1`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	var out []recipient
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("list groups: %w", err)
		}
		j, err := types.ParseJID(id)
		if err != nil || j.Server != types.GroupServer {
			continue
		}
		if name == "" {
			name = id
		}
		out = append(out, recipient{jid: j, name: name, group: true})
	}
	return out, rows.Err()
}

func (c *waConnector) byName(ctx context.Context, name string) (recipient, error) {
	contacts, err := c.dev.Contacts.GetAllContacts(ctx)
	if err != nil {
		return recipient{}, fmt.Errorf("list contacts: %w", err)
	}
	// One person can be listed under both their number and their LID.
	people := make(map[types.JID]bool)
	for j, ci := range contacts {
		if !strings.EqualFold(ci.FullName, name) && !strings.EqualFold(ci.FirstName, name) {
			continue
		}
		pn := j.ToNonAD()
		if pn.Server == types.HiddenUserServer {
			if pn, err = c.dev.LIDs.GetPNForLID(ctx, pn); err != nil {
				return recipient{}, fmt.Errorf("look up phone number: %w", err)
			}
			pn = pn.ToNonAD()
		}
		if pn.Server == types.DefaultUserServer {
			people[pn] = true
		}
	}
	var matches []recipient
	for pn := range people {
		// Resolve as byJID does, so preparing the result again shows the same name.
		r, err := c.byJID(ctx, pn)
		if err != nil {
			return recipient{}, err
		}
		matches = append(matches, r)
	}
	groups, err := c.groups(ctx)
	if err != nil {
		return recipient{}, err
	}
	for _, g := range groups {
		if strings.EqualFold(g.name, name) {
			matches = append(matches, g)
		}
	}

	switch len(matches) {
	case 0:
		return recipient{}, connector.Invalid("no address-book contact or group is named %q", name)
	case 1:
		return matches[0], nil
	}
	// Numbers are masked: the agent may not otherwise see these people, and
	// it can find the JID of the one it means in the chats it can read.
	candidates := make([]string, len(matches))
	for i, m := range matches {
		if m.group {
			candidates[i] = m.name + " (group)"
		} else {
			candidates[i] = m.name + " (" + formatPhone(m.jid.User, true) + ")"
		}
	}
	slices.Sort(candidates)
	return recipient{}, connector.Invalid("%q matches %d chats: %s; retry with the chat JID",
		name, len(matches), strings.Join(candidates, ", "))
}

// Execute never retries: once SendMessage is called the message may have
// been delivered, so any error after that point is Unknown.
func (c *waConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	to, err := types.ParseJID(rec.Args["to"])
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "invalid recipient: " + err.Error()}
	}
	text := rec.Args["text"]
	if !c.cli.IsConnected() || !c.cli.IsLoggedIn() {
		return connector.Result{Status: connector.Failed, Detail: "whatsapp is not connected"}
	}
	resp, err := c.cli.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	res := connector.Result{Status: connector.Done, Ref: resp.ID}
	if err := c.record(ctx, to, text, resp); err != nil {
		c.log.Error("store sent message", "id", resp.ID, "chat", to.String(), "err", err)
		res.Detail = "sent, but not stored: " + err.Error()
	}
	return res
}

func (c *waConnector) record(ctx context.Context, to types.JID, text string, resp whatsmeow.SendResponse) error {
	ct := store.Container{ID: to.String(), Kind: kindFor(to), IsGroup: to.Server == types.GroupServer}
	if !ct.IsGroup {
		name, err := c.addressBookName(ctx, to)
		if err != nil {
			return err
		}
		ct.Name = name
	}
	if err := c.st.UpsertContainer(ctx, ct); err != nil {
		return err
	}
	at := resp.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	msg := store.Message{
		ID:          messageID(to, resp.ID),
		ContainerID: to.String(),
		SenderName:  c.dev.PushName,
		SentAt:      at,
		IsFromMe:    true,
		Body:        text,
	}
	if own := c.dev.GetJID(); !own.IsEmpty() {
		msg.SenderID = own.ToNonAD().String()
	}
	return c.st.UpsertMessage(ctx, msg)
}

// phoneDigits accepts an E.164 number or bare digits including the country
// code, tolerating spaces, dashes, dots, and parentheses.
func phoneDigits(s string) (string, bool) {
	s = strings.TrimPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	d := b.String()
	if len(d) < 7 || len(d) > 15 || d[0] == '0' {
		return "", false
	}
	return d, true
}

// E.164 country codes are prefix-free: 1 and 7 are one digit, these
// two-digit prefixes are complete codes, and every other code has three.
var twoDigitCC = map[string]bool{
	"20": true, "27": true,
	"30": true, "31": true, "32": true, "33": true, "34": true, "36": true, "39": true,
	"40": true, "41": true, "43": true, "44": true, "45": true, "46": true, "47": true, "48": true, "49": true,
	"51": true, "52": true, "53": true, "54": true, "55": true, "56": true, "57": true, "58": true,
	"60": true, "61": true, "62": true, "63": true, "64": true, "65": true, "66": true,
	"81": true, "82": true, "84": true, "86": true,
	"90": true, "91": true, "92": true, "93": true, "94": true, "95": true, "98": true,
}

// formatPhone renders digits as "+1 555 123 4567": the country code, then the
// national number in groups of three ending in four. Masking hides all but
// the last four national digits.
func formatPhone(digits string, mask bool) string {
	cc := 3
	switch {
	case digits == "":
		return ""
	case digits[0] == '1' || digits[0] == '7':
		cc = 1
	case len(digits) >= 2 && twoDigitCC[digits[:2]]:
		cc = 2
	}
	if len(digits) <= cc+4 {
		return "+" + digits
	}
	national := digits[cc:]
	if mask {
		national = strings.Repeat("•", len(national)-4) + national[len(national)-4:]
	}
	// Bytes per digit differ once masked, so group by rune.
	rs := []rune(national)
	head, tail := rs[:len(rs)-4], string(rs[len(rs)-4:])
	var groups []string
	for len(head) > 3 {
		groups = append([]string{string(head[len(head)-3:])}, groups...)
		head = head[:len(head)-3]
	}
	if len(head) > 0 {
		groups = append([]string{string(head)}, groups...)
	}
	return "+" + digits[:cc] + " " + strings.Join(append(groups, tail), " ")
}
