package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

func (c *waConnector) Spec() connector.Spec {
	return connector.Spec{Name: "whatsapp", Ops: []connector.Op{{
		Name:    "send",
		Kind:    connector.Write,
		Summary: "Send a text message to a contact or group.",
		Params: []connector.Param{
			{
				Name:      "to",
				Doc:       "chat JID (…@s.whatsapp.net, …@lid, or …@g.us), phone number with country code, or exact address-book contact or group name",
				Required:  true,
				Container: true,
			},
			{Name: "text", Doc: "message text", Required: true, Editable: true},
		},
	}}, Setup: []connector.Param{
		{Name: "phone", Doc: "Get a pairing code for this number instead of a QR code, e.g. +15551234567"},
	}}
}

type recipient struct {
	jid     types.JID
	aliases []string // the contact's LID
	name    string   // address-book or group name; empty for numbers not in the address book
	group   bool
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

// Numbers are masked: the agent may not otherwise see these people, and it
// can find the JID of the one it means in the chats it can read.
func (r recipient) candidate() connector.Candidate {
	label := r.name + " (group)"
	if !r.group {
		label = r.name + " (" + formatPhone(r.jid.User, true) + ")"
	}
	return connector.Candidate{ID: r.jid.String(), Aliases: r.aliases, Label: label}
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
	_, dev, linked := c.current()
	if !linked {
		return connector.Prepared{}, &connector.Error{Status: http.StatusServiceUnavailable, Message: "whatsapp is not linked"}
	}
	r, err := book{dev: dev, st: c.st}.resolve(ctx, args["to"])
	if err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args:      map[string]string{"to": r.jid.String(), "text": text},
		Display:   []approval.Field{r.field(), {Label: "Text", Value: text, Param: "text"}},
		Container: r.jid.String(),
		Aliases:   r.aliases,
	}, nil
}

// book resolves recipients from local state only: the address book and LID
// map in the session database and the groups in data.db.
type book struct {
	dev *wastore.Device
	st  *store.Store
}

// Phone numbers are not checked with IsOnWhatsApp: that is a network query
// WhatsApp rate-limits and watches for number scraping, and Prepare runs for
// every proposal and again at execution. Sending to a number without WhatsApp
// fails upstream instead.
func (b book) resolve(ctx context.Context, to string) (recipient, error) {
	to = strings.TrimSpace(to)
	switch {
	case to == "":
		return recipient{}, connector.Invalid("to is required")
	case strings.Contains(to, "@"):
		j, err := types.ParseJID(to)
		if err != nil {
			return recipient{}, connector.Invalid("invalid JID %q", to)
		}
		return b.byJID(ctx, j.ToNonAD())
	}
	if digits, ok := phoneDigits(to); ok {
		return b.byJID(ctx, types.NewJID(digits, types.DefaultUserServer))
	}
	return b.byName(ctx, to)
}

func (b book) byJID(ctx context.Context, j types.JID) (recipient, error) {
	switch j.Server {
	case types.DefaultUserServer:
		if d, ok := phoneDigits(j.User); !ok || d != j.User {
			return recipient{}, connector.Invalid("invalid user JID %q", j.String())
		}
		r := recipient{jid: j}
		// The gateway refuses the send if the LID is hidden, so a person
		// hidden by either ID stays unsendable.
		lid, err := b.dev.LIDs.GetLIDForPN(ctx, j)
		if err != nil {
			return recipient{}, fmt.Errorf("look up lid: %w", err)
		}
		if !lid.IsEmpty() {
			r.aliases = []string{lid.ToNonAD().String()}
		}
		if r.name, err = b.addressBookName(ctx, j, lid); err != nil {
			return recipient{}, err
		}
		return r, nil
	case types.HiddenUserServer:
		pn, err := b.dev.LIDs.GetPNForLID(ctx, j)
		if err != nil {
			return recipient{}, fmt.Errorf("look up phone number: %w", err)
		}
		if pn.IsEmpty() {
			return recipient{}, connector.Invalid("no phone number is known for %s; use a phone number or name", j.String())
		}
		return b.byJID(ctx, pn.ToNonAD())
	case types.GroupServer:
		groups, err := b.groups(ctx)
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
	return recipient{}, connector.Invalid("cannot send to %s: use a phone number, a name, or a …@s.whatsapp.net, …@lid, or …@g.us JID", j.String())
}

// Push names are chosen by the sender and never identify a recipient.
func addressBookName(ci types.ContactInfo) string {
	if ci.FullName != "" {
		return ci.FullName
	}
	return ci.FirstName
}

// Address-book entries may be keyed by phone number or by LID.
func (b book) addressBookName(ctx context.Context, pn, lid types.JID) (string, error) {
	ci, err := b.dev.Contacts.GetContact(ctx, pn)
	if err != nil {
		return "", fmt.Errorf("look up contact: %w", err)
	}
	if n := addressBookName(ci); n != "" || lid.IsEmpty() {
		return n, nil
	}
	ci, err = b.dev.Contacts.GetContact(ctx, lid)
	if err != nil {
		return "", fmt.Errorf("look up contact: %w", err)
	}
	return addressBookName(ci), nil
}

func (b book) groups(ctx context.Context) ([]recipient, error) {
	rows, err := b.st.DB().QueryContext(ctx,
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

func (b book) byName(ctx context.Context, name string) (recipient, error) {
	contacts, err := b.dev.Contacts.GetAllContacts(ctx)
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
			if pn, err = b.dev.LIDs.GetPNForLID(ctx, pn); err != nil {
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
		r, err := b.byJID(ctx, pn)
		if err != nil {
			return recipient{}, err
		}
		matches = append(matches, r)
	}
	groups, err := b.groups(ctx)
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
	candidates := make([]connector.Candidate, len(matches))
	for i, m := range matches {
		candidates[i] = m.candidate()
	}
	slices.SortFunc(candidates, func(a, b connector.Candidate) int { return cmp.Compare(a.Label, b.Label) })
	return recipient{}, &connector.Error{
		Status:     http.StatusBadRequest,
		Message:    fmt.Sprintf("several chats match %q; retry with one of the candidates' IDs", name),
		Candidates: candidates,
	}
}

// Execute never retries: once SendMessage is called the message may have
// been delivered, so any error after that point is Unknown.
func (c *waConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	to, err := types.ParseJID(rec.Args["to"])
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: "invalid recipient: " + err.Error()}
	}
	text := rec.Args["text"]
	cli, dev, _ := c.current()
	if !cli.IsConnected() || !cli.IsLoggedIn() {
		return connector.Result{Status: connector.Failed, Detail: "whatsapp is not connected"}
	}
	resp, err := cli.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	res := connector.Result{Status: connector.Done, Ref: resp.ID}
	if err := (book{dev: dev, st: c.st}).record(ctx, to, text, resp); err != nil {
		c.log.Error("store sent message", "id", resp.ID, "chat", to.String(), "err", err)
		res.Detail = "sent, but not stored: " + err.Error()
	}
	return res
}

func (b book) record(ctx context.Context, to types.JID, text string, resp whatsmeow.SendResponse) error {
	ct := store.Container{ID: to.String(), Kind: kindFor(to), IsGroup: to.Server == types.GroupServer}
	if !ct.IsGroup {
		lid, err := b.dev.LIDs.GetLIDForPN(ctx, to)
		if err != nil {
			return fmt.Errorf("look up lid: %w", err)
		}
		if ct.Name, err = b.addressBookName(ctx, to, lid); err != nil {
			return err
		}
	}
	if err := b.st.UpsertContainer(ctx, ct); err != nil {
		return err
	}
	at := resp.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	msg := store.Message{
		ID:          messageID(to, resp.ID),
		ContainerID: to.String(),
		SenderName:  b.dev.PushName,
		SentAt:      at,
		IsFromMe:    true,
		Body:        text,
	}
	if own := b.dev.GetJID(); !own.IsEmpty() {
		msg.SenderID = own.ToNonAD().String()
	}
	return b.st.UpsertMessage(ctx, msg)
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
