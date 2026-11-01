package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

var fromParam = connector.Param{Name: "from", Doc: "one of the owner's sending addresses; default their own address, or for a reply the one the email was sent to"}

var spec = connector.Spec{
	Name: "fastmail",
	Doc: "Chats are email threads. The last 90 days of mail arrive by the first sync, new mail within a minute; " +
		"Trash, Spam and Drafts are left out. A body cut at 16 KiB ends in " + truncatedMarker + ".\n" +
		"send and reply propose an email; it goes out from the owner's account once they approve it.",
	Setup: setupParams,
	Ops: []connector.Op{
		{
			Name: "fetch", Kind: connector.Read,
			Summary: "Read the whole text of an email whose stored body is truncated.",
			Params: []connector.Param{
				{Name: "chat", Doc: "thread ID from mandate fastmail chats", Required: true, Container: true},
				{Name: "seq", Doc: "the email's seq", Required: true},
			},
		},
		{
			Name: "send", Kind: connector.Write,
			Summary: "Send a new email.",
			Params: []connector.Param{
				{Name: "to", Doc: "recipient addresses, comma-separated", Required: true},
				{Name: "cc", Doc: "addresses to copy, comma-separated"},
				{Name: "subject", Doc: "subject line", Required: true, Editable: true},
				{Name: "body", Doc: "plain text", Required: true, Editable: true},
				fromParam,
			},
		},
		{
			Name: "reply", Kind: connector.Write,
			Summary: "Reply to an email in a thread: to its Reply-To or sender, or to the recipients of the owner's own email.",
			Params: []connector.Param{
				{Name: "chat", Doc: "thread ID from mandate fastmail chats", Required: true, Container: true},
				{Name: "seq", Doc: "the email's seq; default the newest one not from the owner"},
				{Name: "body", Doc: "plain text", Required: true, Editable: true},
				fromParam,
			},
		},
	},
}

func (c *fmConnector) Spec() connector.Spec { return spec }

var (
	errNotLinked  = &connector.Error{Status: http.StatusServiceUnavailable, Message: "fastmail is not linked"}
	errConnecting = &connector.Error{Status: http.StatusServiceUnavailable, Message: "fastmail is connecting to Fastmail; try again in a minute"}
)

// linked returns the account's settings once its session is known.
func (c *fmConnector) linked() (*account, *conn, error) {
	a := c.current()
	if a == nil {
		return nil, nil, errNotLinked
	}
	cn, _, _ := a.state()
	if cn == nil {
		return nil, nil, errConnecting
	}
	return a, cn, nil
}

func only(args map[string]string, names ...string) error {
	for k := range args {
		if !slices.Contains(names, k) {
			return connector.Invalid("unknown arg %q", k)
		}
	}
	return nil
}

func (c *fmConnector) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
	switch op {
	case "send":
		if err := only(args, "from", "to", "cc", "subject", "body"); err != nil {
			return connector.Prepared{}, err
		}
	case "reply":
		// to and subject follow the email; they come back in Args to show
		// the owner, so a prepared reply must be accepted again.
		if err := only(args, "chat", "seq", "body", "from", "to", "subject"); err != nil {
			return connector.Prepared{}, err
		}
	default:
		return connector.Prepared{}, connector.ErrUnknownOp
	}
	if strings.TrimSpace(args["body"]) == "" {
		return connector.Prepared{}, connector.Invalid("body is required")
	}
	_, cn, err := c.linked()
	if err != nil {
		return connector.Prepared{}, err
	}
	if op == "send" {
		return prepareSend(cn, args)
	}
	return c.prepareReply(ctx, cn, args)
}

func prepareSend(cn *conn, args map[string]string) (connector.Prepared, error) {
	from, err := sender(cn, args["from"])
	if err != nil {
		return connector.Prepared{}, err
	}
	to, err := recipients("to", args["to"])
	if err != nil {
		return connector.Prepared{}, err
	}
	if to == "" {
		return connector.Prepared{}, connector.Invalid("to is required")
	}
	cc, err := recipients("cc", args["cc"])
	if err != nil {
		return connector.Prepared{}, err
	}
	subject := strings.TrimSpace(args["subject"])
	switch {
	case subject == "":
		return connector.Prepared{}, connector.Invalid("subject is required")
	case strings.ContainsAny(subject, "\r\n"):
		return connector.Prepared{}, connector.Invalid("subject must be one line")
	}
	out := map[string]string{"from": from, "to": to, "subject": subject, "body": args["body"]}
	display := []approval.Field{{Label: "From", Value: from}, {Label: "To", Value: to}}
	if cc != "" {
		out["cc"] = cc
		display = append(display, approval.Field{Label: "Cc", Value: cc})
	}
	display = append(display,
		approval.Field{Label: "Subject", Value: subject, Param: "subject"},
		approval.Field{Label: "Text", Value: args["body"], Param: "body"})
	return connector.Prepared{Args: out, Display: display}, nil
}

func (c *fmConnector) prepareReply(ctx context.Context, cn *conn, args map[string]string) (connector.Prepared, error) {
	chat := args["chat"]
	if chat == "" {
		return connector.Prepared{}, connector.Invalid("chat is required")
	}
	var seq int64
	var replyTo, subject, rcpts string
	q := `SELECT m.seq, e.reply_to, e.subject, e.recipients FROM messages AS m JOIN fastmail_emails AS e ON e.id = m.id
WHERE m.container_id = ?1 ORDER BY m.deleted, m.is_from_me, m.sent_at DESC, m.seq DESC LIMIT 1`
	qargs := []any{chat}
	if s := args["seq"]; s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			return connector.Prepared{}, connector.Invalid("seq must be a seq from mandate's output")
		}
		q = `SELECT m.seq, e.reply_to, e.subject, e.recipients FROM messages AS m JOIN fastmail_emails AS e ON e.id = m.id
WHERE m.container_id = ?1 AND m.seq = ?2`
		qargs = append(qargs, n)
	}
	err := c.st.DB().QueryRowContext(ctx, q, qargs...).Scan(&seq, &replyTo, &subject, &rcpts)
	switch {
	case errors.Is(err, sql.ErrNoRows) && len(qargs) > 1:
		return connector.Prepared{}, connector.Invalid("no email with seq %s in chat %s", args["seq"], chat)
	case errors.Is(err, sql.ErrNoRows):
		return connector.Prepared{}, connector.Invalid("unknown chat %q: run mandate fastmail chats", chat)
	case err != nil:
		return connector.Prepared{}, fmt.Errorf("look up the email: %w", err)
	}
	to, err := recipients("to", replyTo)
	if err != nil || to == "" {
		return connector.Prepared{}, connector.Invalid("the email has no address to reply to")
	}
	subject = reSubject(subject)
	if v, ok := args["to"]; ok && v != to {
		return connector.Prepared{}, connector.Invalid("a reply goes to %s; use send to write to others", to)
	}
	if v, ok := args["subject"]; ok && v != subject {
		return connector.Prepared{}, connector.Invalid("a reply's subject is %q; use send for another", subject)
	}
	from := args["from"]
	if from == "" {
		from = replyFrom(cn, rcpts)
	}
	if from, err = sender(cn, from); err != nil {
		return connector.Prepared{}, err
	}
	return connector.Prepared{
		Args: map[string]string{
			"chat": chat, "seq": strconv.FormatInt(seq, 10),
			"from": from, "to": to, "subject": subject, "body": args["body"],
		},
		Display: []approval.Field{
			{Label: "From", Value: from},
			{Label: "To", Value: to},
			{Label: "Subject", Value: subject},
			{Label: "Text", Value: args["body"], Param: "body"},
		},
		Container: chat,
	}, nil
}

// sender checks that s is an address the account sends as; empty is its
// default identity.
func sender(cn *conn, s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		id, ok := cn.defaultIdentity()
		if !ok {
			return "", connector.Invalid("the account has no address to send from")
		}
		return id.Email, nil
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return "", connector.Invalid("from: %v", err)
	}
	if _, ok := cn.identity(a.Address); !ok {
		return "", connector.Invalid("from: %s is not one of the account's sending addresses", a.Address)
	}
	return a.Address, nil
}

// replyFrom is the sending address among an email's recipients, if any.
func replyFrom(cn *conn, rcpts string) string {
	for r := range strings.SplitSeq(rcpts, ",") {
		if id, ok := cn.identity(r); ok && r != "" {
			if strings.HasPrefix(id.Email, "*@") {
				return r
			}
			return id.Email
		}
	}
	return ""
}

// recipients normalizes a comma-separated address list to bare addresses.
func recipients(param, s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	list, err := mail.ParseAddressList(s)
	if err != nil {
		return "", connector.Invalid("%s: %v", param, err)
	}
	addrs := make([]string, len(list))
	for i, a := range list {
		domain := a.Address[strings.LastIndexByte(a.Address, '@')+1:]
		if !strings.Contains(domain, ".") {
			return "", connector.Invalid("%s: %s has no domain like example.com", param, a.Address)
		}
		addrs[i] = a.Address
	}
	return strings.Join(addrs, ", "), nil
}

func reSubject(s string) string {
	s = oneLine(s)
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return strings.TrimSpace("Re: " + s)
}

// Execute creates the email in Drafts and submits it in one request, which
// moves it to Sent once it is sent. Only an answer that the request was
// refused, or that Fastmail did not send it, is Failed; anything else may
// have sent it, so it is Unknown.
func (c *fmConnector) Execute(ctx context.Context, rec approval.Record) connector.Result {
	failed := func(format string, a ...any) connector.Result {
		return connector.Result{Status: connector.Failed, Detail: fmt.Sprintf(format, a...)}
	}
	a, cn, err := c.linked()
	if err != nil {
		return failed("%v", err)
	}
	from := rec.Args["from"]
	id, ok := cn.identity(from)
	if !ok {
		return failed("%s is no longer one of the account's sending addresses", from)
	}
	drafts, sent := cn.roles["drafts"], cn.roles["sent"]
	if drafts == "" || sent == "" {
		return failed("the account has no Drafts or Sent mailbox")
	}
	draft := map[string]any{
		"mailboxIds": map[string]bool{drafts: true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []address{{Name: id.Name, Email: from}},
		"subject":    rec.Args["subject"],
		"bodyValues": map[string]any{"body": map[string]string{"value": rec.Args["body"]}},
		"textBody":   []bodyPart{{PartID: "body", Type: "text/plain"}},
	}
	for _, field := range []string{"to", "cc"} {
		if rec.Args[field] == "" {
			continue
		}
		list, err := mail.ParseAddressList(rec.Args[field])
		if err != nil {
			return failed("%s: %v", field, err)
		}
		as := make([]address, len(list))
		for i, l := range list {
			as[i] = address{Email: l.Address}
		}
		draft[field] = as
	}
	if rec.Op == "reply" {
		var messageID, refs string
		err := c.st.DB().QueryRowContext(ctx, `SELECT e.message_id, e.refs FROM fastmail_emails AS e JOIN messages AS m ON m.id = e.id
WHERE m.seq = ?1 AND m.container_id = ?2`, rec.Args["seq"], rec.Args["chat"]).Scan(&messageID, &refs)
		if err != nil {
			return failed("look up the email: %v", err)
		}
		if messageID != "" {
			draft["inReplyTo"] = []string{messageID}
		}
		if refs != "" {
			draft["references"] = strings.Fields(refs)
		}
	}

	resps, err := a.call(ctx, cn.api,
		invocation{"Email/set", map[string]any{"accountId": cn.accountID, "create": map[string]any{"draft": draft}}, "0"},
		invocation{"EmailSubmission/set", map[string]any{
			"accountId": cn.accountID,
			"create":    map[string]any{"send": map[string]string{"identityId": id.ID, "emailId": "#draft"}},
			"onSuccessUpdateEmail": map[string]any{"#send": map[string]any{
				"mailboxIds/" + drafts: nil, "mailboxIds/" + sent: true, "keywords/$draft": nil,
			}},
		}, "1"},
	)
	var se *statusError
	switch {
	case errors.Is(err, errUnauthorized):
		return failed("%v", err)
	case errors.As(err, &se) && se.code < 500:
		return failed("%v", err)
	case err != nil:
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	var me *methodError
	created, err := result[setResponse](resps, "Email/set", "0")
	switch {
	case errors.As(err, &me):
		return failed("Fastmail refused the email: %v", err)
	case err != nil:
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	if e, ok := created.NotCreated["draft"]; ok {
		return failed("Fastmail refused the email: %s", e)
	}
	emailID := created.Created["draft"].ID
	submitted, err := result[setResponse](resps, "EmailSubmission/set", "1")
	switch {
	case errors.As(err, &me):
		c.discard(ctx, a, cn, emailID)
		return failed("Fastmail did not send it: %v", err)
	case err != nil:
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	if e, ok := submitted.NotCreated["send"]; ok {
		c.discard(ctx, a, cn, emailID)
		return failed("Fastmail did not send it: %s", e)
	}
	if _, ok := submitted.Created["send"]; !ok {
		return connector.Result{Status: connector.Unknown, Detail: "Fastmail's answer does not say whether it was sent"}
	}
	a.poke()
	return connector.Result{Status: connector.Done, Ref: emailID}
}

// discard deletes the draft of an email Fastmail did not send.
func (c *fmConnector) discard(ctx context.Context, a *account, cn *conn, id string) {
	if id == "" {
		return
	}
	resps, err := a.call(ctx, cn.api, invocation{"Email/set", map[string]any{"accountId": cn.accountID, "destroy": []string{id}}, "0"})
	if err == nil {
		_, err = result[setResponse](resps, "Email/set", "0")
	}
	if err != nil {
		c.log.Warn("delete the unsent draft", "id", id, "err", err)
	}
}
