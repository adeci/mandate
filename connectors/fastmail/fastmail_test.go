package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

type fixture struct {
	t   *testing.T
	jm  *fakeJMAP
	env *connector.Env
	c   *fmConnector
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, jm: newFakeJMAP(t)}
	f.env = &connector.Env{StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)}
	if err := os.MkdirAll(f.env.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	f.start()
	return f
}

// start runs the connector on the state directory, as after a restart.
func (f *fixture) start() {
	f.t.Helper()
	st, err := store.Open(context.Background(), f.env.DataPath())
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := newConnector(context.Background(), st, f.env.Secret("token"), f.env.Log,
		f.jm.srv.Client(), f.jm.srv.URL+"/jmap/session", time.Hour)
	if err != nil {
		f.t.Fatal(err)
	}
	f.c = c
	f.t.Cleanup(func() { c.Close() })
}

func (f *fixture) link() {
	f.t.Helper()
	step, err := f.c.Setup(context.Background(), map[string]string{"token": " " + goodToken + "\n"})
	if err != nil || !step.Done {
		f.t.Fatalf("setup: %+v %v", step, err)
	}
	f.synced()
}

// synced waits until the connector has caught up with the account.
func (f *fixture) synced() {
	f.t.Helper()
	want := f.jm.currentState()
	f.c.current().poke()
	eventually(f.t, "synced to state "+want, func() bool {
		var got string
		f.c.st.DB().QueryRow(`SELECT email_state FROM fastmail_sync WHERE account_id = ?1`, accountID).Scan(&got)
		return got == want
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

type storedMessage struct {
	Chat, SenderID, Sender, Body, ReplyTo string
	FromMe, Deleted                       bool
}

func (f *fixture) message(id string) storedMessage {
	f.t.Helper()
	var m storedMessage
	err := f.c.st.DB().QueryRow(`SELECT container_id, COALESCE(sender_id, ''), COALESCE(sender_name, ''), COALESCE(body, ''),
COALESCE(reply_to_id, ''), is_from_me, deleted FROM messages WHERE id = ?1`, id).
		Scan(&m.Chat, &m.SenderID, &m.Sender, &m.Body, &m.ReplyTo, &m.FromMe, &m.Deleted)
	if err != nil {
		f.t.Fatalf("message %s: %v", id, err)
	}
	return m
}

func (f *fixture) seq(id string) string {
	f.t.Helper()
	var seq string
	if err := f.c.st.DB().QueryRow(`SELECT seq FROM messages WHERE id = ?1`, id).Scan(&seq); err != nil {
		f.t.Fatalf("seq of %s: %v", id, err)
	}
	return seq
}

func textEmail(id, thread string, from address, to []address, subject, text string, at time.Time, boxes ...string) email {
	e := email{
		ID: id, ThreadID: thread, From: []address{from}, To: to, Subject: subject, ReceivedAt: at,
		MailboxIDs: map[string]bool{}, Keywords: map[string]bool{},
		MessageID:  []string{id + "@mail.example"},
		TextBody:   []bodyPart{{PartID: "1", Type: "text/plain"}},
		BodyValues: map[string]bodyValue{"1": {Value: text}},
	}
	for _, b := range boxes {
		e.MailboxIDs[b] = true
	}
	return e
}

var (
	alice = address{Name: "Alice Doe", Email: "Alice@Example.com"}
	owner = address{Name: "Owner Name", Email: "owner@fastmail.com"}
	now   = time.Now().UTC().Truncate(time.Second)
)

// seed fills the account: a thread with the owner's answer, an HTML email to
// a wildcard alias, and mail the sync leaves out.
func seed(jm *fakeJMAP) {
	jm.add(textEmail("e1", "t1", alice, []address{owner}, "Lunch?", "Lunch on Friday?\r\n\r\n\r\n\r\nAlice", now.Add(-48*time.Hour), "mb-inbox"))
	e2 := textEmail("e2", "t1", owner, []address{alice}, "Re: Lunch?", "Sure.", now.Add(-47*time.Hour), "mb-sent")
	e2.InReplyTo, e2.References = []string{"e1@mail.example"}, []string{"e1@mail.example"}
	jm.add(e2)
	e3 := textEmail("e3", "t2", address{Name: "Shop", Email: "news@shop.example"}, []address{{Email: "hi@owner.dev"}}, "Deals", "", now.Add(-24*time.Hour), "mb-archive")
	e3.ReplyTo = []address{{Email: "deals@shop.example"}}
	e3.TextBody = []bodyPart{{PartID: "1", Type: "text/html"}}
	e3.BodyValues["1"] = bodyValue{Value: `<html><head><style>p{color:red}</style></head><body>` +
		`<p>Big &amp; bold</p><p>See <a href="https://shop.example/x">this</a></p></body></html>`}
	e3.Attachments = []bodyPart{{Type: "application/pdf", Name: "invoice.pdf", BlobID: "b-invoice", Size: 7}}
	jm.add(e3)
	jm.add(textEmail("trashed", "t3", alice, []address{owner}, "old news", "x", now.Add(-time.Hour), "mb-trash"))
	jm.add(textEmail("ancient", "t4", alice, []address{owner}, "2025", "x", now.Add(-200*24*time.Hour), "mb-inbox"))
	draft := textEmail("draft", "t5", owner, []address{alice}, "unsent", "x", now.Add(-time.Hour), "mb-drafts")
	draft.Keywords["$draft"] = true
	jm.add(draft)
}

func TestSetup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	if h := f.c.Health(ctx); h != (connector.Health{State: connector.NeedsSetup, Detail: "not linked"}) {
		t.Fatalf("health before linking: %+v", h)
	}
	for name, tc := range map[string]struct {
		token string
		fake  func(*fakeJMAP)
		want  string
	}{
		"wrong token":      {"fmu1-wrong", nil, "refused the token"},
		"no submission":    {goodToken, func(j *fakeJMAP) { j.noSubmission = true }, "scopes Email and Email submission"},
		"read-only access": {goodToken, func(j *fakeJMAP) { j.noSubmission, j.readOnly = false, true }, "without Read-only access"},
	} {
		if tc.fake != nil {
			f.jm.set(tc.fake)
		}
		_, err := f.c.Setup(ctx, map[string]string{"token": tc.token})
		var ce *connector.Error
		if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if entries, _ := os.ReadDir(f.env.PrivateDir()); len(entries) != 0 {
			t.Fatalf("%s: stored %v", name, entries)
		}
		if f.c.current() != nil {
			t.Fatalf("%s: linked", name)
		}
	}
	f.jm.set(func(j *fakeJMAP) { j.noSubmission, j.readOnly = false, false })

	f.link()
	if h := f.c.Health(ctx); h.State != connector.Healthy {
		t.Fatalf("health after linking: %+v", h)
	}
	if b, err := os.ReadFile(filepath.Join(f.env.PrivateDir(), "token")); err != nil || string(b) != goodToken {
		t.Fatalf("stored token %q %v", b, err)
	}
	var ce *connector.Error
	if _, err := f.c.Setup(ctx, map[string]string{"token": goodToken}); !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("second setup: %v", err)
	}

	// A restart resumes with the stored token.
	f.c.Close()
	f.start()
	eventually(t, "the restarted connector connects", func() bool { return f.c.Health(ctx) == connector.Health{State: connector.Healthy} })

	if err := f.c.Unlink(ctx); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(f.env.PrivateDir()); len(entries) != 0 {
		t.Fatalf("private dir keeps %v", entries)
	}
	if h := f.c.Health(ctx); h.State != connector.NeedsSetup {
		t.Fatalf("health after unlink: %+v", h)
	}
	if _, err := f.c.Prepare(ctx, "send", map[string]string{"to": "a@example.com", "subject": "s", "body": "b"}); !errors.Is(err, errNotLinked) {
		t.Fatalf("prepare after unlink: %v", err)
	}
}

func TestRevokedToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.link()
	f.jm.set(func(j *fakeJMAP) { j.token = "fmu1-other" })
	f.c.current().poke()
	eventually(t, "health is down", func() bool { return f.c.Health(context.Background()).State == connector.Broken })
	if h := f.c.Health(context.Background()); !strings.Contains(h.Detail, "revoked") {
		t.Fatalf("health = %+v", h)
	}
}

func TestSync(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seed(f.jm)
	f.jm.set(func(j *fakeJMAP) { j.queryCap = 2 }) // the first sync pages
	f.link()

	want := map[string]storedMessage{
		"e1": {
			Chat: "t1", SenderID: "alice@example.com", Sender: "Alice Doe",
			Body: "Subject: Lunch?\nTo: Owner Name <owner@fastmail.com>\n\nLunch on Friday?\n\nAlice",
		},
		"e2": {
			Chat: "t1", SenderID: "owner@fastmail.com", Sender: "Owner Name", FromMe: true, ReplyTo: "e1",
			Body: "Subject: Re: Lunch?\nTo: Alice Doe <Alice@Example.com>\n\nSure.",
		},
		"e3": {
			Chat: "t2", SenderID: "news@shop.example", Sender: "Shop",
			Body: "Subject: Deals\nTo: hi@owner.dev\n\nBig & bold\n\nSee this (https://shop.example/x)",
		},
	}
	for id, w := range want {
		if got := f.message(id); got != w {
			t.Errorf("%s:\n got %+v\nwant %+v", id, got, w)
		}
	}
	var n int
	f.c.st.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != len(want) {
		t.Fatalf("stored %d messages, want %d (no trash, drafts or mail older than 90 days)", n, len(want))
	}
	type thread struct{ Name, Last string }
	threads := func() map[string]thread {
		got := map[string]thread{}
		rows, err := f.c.st.DB().Query(`SELECT id, name, last_message_at FROM containers WHERE kind = 'thread'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var th thread
			rows.Scan(&id, &th.Name, &th.Last)
			got[id] = th
		}
		return got
	}
	ts := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z") }
	if got := threads(); !reflect.DeepEqual(got, map[string]thread{
		"t1": {"Lunch?", ts(now.Add(-47 * time.Hour))},
		"t2": {"Deals", ts(now.Add(-24 * time.Hour))},
	}) {
		t.Fatalf("threads %+v", got)
	}

	// New mail, mail moved to Trash, and mail deleted for good.
	e4 := textEmail("e4", "t1", alice, []address{owner}, "Re: Lunch?", "Friday works", now, "mb-inbox")
	e4.InReplyTo = []string{"e2@mail.example"}
	f.jm.add(e4)
	f.jm.update("e1", func(e *email) { e.MailboxIDs = map[string]bool{"mb-trash": true} })
	f.jm.destroy("e3")
	f.jm.update("ancient", func(e *email) { e.Keywords["$flagged"] = true })
	f.synced()
	if got := f.message("e4"); got.ReplyTo != "e2" || got.Body != "Subject: Re: Lunch?\nTo: Owner Name <owner@fastmail.com>\n\nFriday works" {
		t.Fatalf("e4 = %+v", got)
	}
	if !f.message("e1").Deleted || !f.message("e3").Deleted {
		t.Fatal("trashed and destroyed mail is not marked deleted")
	}
	if err := f.c.st.DB().QueryRow(`SELECT 1 FROM messages WHERE id = 'ancient'`).Scan(new(int)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an old email stored after an update: %v", err)
	}
	if got := threads()["t1"].Last; got != ts(now) {
		t.Fatalf("t1 last message at %s", got)
	}

	// Back out of Trash; then Fastmail forgets the old state.
	f.jm.update("e1", func(e *email) { e.MailboxIDs = map[string]bool{"mb-inbox": true} })
	f.jm.set(func(j *fakeJMAP) { j.minState = j.state + 1 })
	f.jm.add(textEmail("e5", "t6", alice, []address{owner}, "Hello", "hi", now, "mb-inbox"))
	f.synced()
	if f.message("e1").Deleted {
		t.Fatal("e1 still deleted after leaving Trash")
	}
	if got := f.message("e5"); got.Chat != "t6" {
		t.Fatalf("e5 after a full resync: %+v", got)
	}
}

func TestFetch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	long := strings.Repeat("all work and no play ", 1000) // 21 000 bytes
	f.jm.add(textEmail("big", "t1", alice, []address{owner}, "Novel", long, now, "mb-inbox"))
	f.link()
	stored := f.message("big").Body
	if !strings.HasSuffix(stored, "\n\n"+truncatedMarker) || len(stored) > maxText+200 {
		t.Fatalf("stored %d bytes ending %q", len(stored), stored[len(stored)-60:])
	}
	ctx := context.Background()
	var ce *connector.Error
	if _, err := f.c.Read(ctx, "fetch", map[string]string{"chat": "t1", "seq": "99"}); !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("unknown seq: %v", err)
	}
	raw, err := f.c.Read(ctx, "fetch", map[string]string{"chat": "t1", "seq": f.seq("big")})
	if err != nil {
		t.Fatal(err)
	}
	var res connector.ReadResult
	if err := json.Unmarshal(raw, &res); err != nil || !reflect.DeepEqual(res.MessageIDs, []string{"big"}) {
		t.Fatalf("result %s %v", raw, err)
	}
	if got := f.message("big").Body; got != "Subject: Novel\nTo: Owner Name <owner@fastmail.com>\n\n"+strings.TrimSpace(long) {
		t.Fatalf("fetched body of %d bytes ends %q", len(got), got[len(got)-60:])
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seed(f.jm)
	f.link()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		op   string
		args map[string]string
		want connector.Prepared
	}{
		"send": {
			op: "send",
			args: map[string]string{
				"to": "Bob <bob@example.com>, carol@example.org", "cc": " dave@example.net ",
				"subject": " Plans ", "body": "See you there.\n",
			},
			want: connector.Prepared{
				Args: map[string]string{
					"from": "owner@fastmail.com", "to": "bob@example.com, carol@example.org", "cc": "dave@example.net",
					"subject": "Plans", "body": "See you there.\n",
				},
				Display: []approval.Field{
					{Label: "From", Value: "owner@fastmail.com"},
					{Label: "To", Value: "bob@example.com, carol@example.org"},
					{Label: "Cc", Value: "dave@example.net"},
					{Label: "Subject", Value: "Plans", Param: "subject"},
					{Label: "Text", Value: "See you there.\n", Param: "body"},
				},
			},
		},
		"send from an alias": {
			op:   "send",
			args: map[string]string{"from": "Me <shop@owner.dev>", "to": "bob@example.com", "subject": "Hi", "body": "b"},
			want: connector.Prepared{
				Args: map[string]string{"from": "shop@owner.dev", "to": "bob@example.com", "subject": "Hi", "body": "b"},
				Display: []approval.Field{
					{Label: "From", Value: "shop@owner.dev"},
					{Label: "To", Value: "bob@example.com"},
					{Label: "Subject", Value: "Hi", Param: "subject"},
					{Label: "Text", Value: "b", Param: "body"},
				},
			},
		},
		"reply goes to the newest email not from the owner": {
			op:   "reply",
			args: map[string]string{"chat": "t1", "body": "Great"},
			want: connector.Prepared{
				Args: map[string]string{
					"chat": "t1", "seq": f.seq("e1"), "from": "owner@fastmail.com",
					"to": "Alice@Example.com", "subject": "Re: Lunch?", "body": "Great",
				},
				Display: []approval.Field{
					{Label: "From", Value: "owner@fastmail.com"},
					{Label: "To", Value: "Alice@Example.com"},
					{Label: "Subject", Value: "Re: Lunch?"},
					{Label: "Text", Value: "Great", Param: "body"},
				},
				Container: "t1",
			},
		},
		"reply to Reply-To from the alias it was sent to": {
			op:   "reply",
			args: map[string]string{"chat": "t2", "seq": f.seq("e3"), "body": "Stop"},
			want: connector.Prepared{
				Args: map[string]string{
					"chat": "t2", "seq": f.seq("e3"), "from": "hi@owner.dev",
					"to": "deals@shop.example", "subject": "Re: Deals", "body": "Stop",
				},
				Display: []approval.Field{
					{Label: "From", Value: "hi@owner.dev"},
					{Label: "To", Value: "deals@shop.example"},
					{Label: "Subject", Value: "Re: Deals"},
					{Label: "Text", Value: "Stop", Param: "body"},
				},
				Container: "t2",
			},
		},
	} {
		p, err := f.c.Prepare(ctx, tc.op, tc.args)
		if err != nil || !reflect.DeepEqual(p, tc.want) {
			t.Fatalf("%s:\n got %+v %v\nwant %+v", name, p, err, tc.want)
		}
		again, err := f.c.Prepare(ctx, tc.op, p.Args)
		if err != nil || !reflect.DeepEqual(again, p) {
			t.Fatalf("%s: preparing the prepared args gave %+v %v", name, again, err)
		}
	}

	for name, tc := range map[string]struct {
		op   string
		args map[string]string
		want string
	}{
		"bad address":       {"send", map[string]string{"to": "bob", "subject": "s", "body": "b"}, "to:"},
		"no domain":         {"send", map[string]string{"to": "bob@localhost", "subject": "s", "body": "b"}, "has no domain"},
		"bad cc":            {"send", map[string]string{"to": "bob@example.com", "cc": "x@", "subject": "s", "body": "b"}, "cc:"},
		"two-line subject":  {"send", map[string]string{"to": "bob@example.com", "subject": "a\nBcc: x@example.com", "body": "b"}, "one line"},
		"foreign from":      {"send", map[string]string{"from": "boss@example.com", "to": "bob@example.com", "subject": "s", "body": "b"}, "not one of"},
		"blank body":        {"send", map[string]string{"to": "bob@example.com", "subject": "s", "body": " \n"}, "body is required"},
		"unknown arg":       {"send", map[string]string{"to": "bob@example.com", "subject": "s", "body": "b", "bcc": "x@example.com"}, "unknown arg"},
		"reply elsewhere":   {"reply", map[string]string{"chat": "t1", "body": "b", "to": "mallory@example.com"}, "use send"},
		"reply to unknown":  {"reply", map[string]string{"chat": "t9", "body": "b"}, "unknown chat"},
		"seq of another":    {"reply", map[string]string{"chat": "t1", "seq": f.seq("e3"), "body": "b"}, "no email with seq"},
		"subject rewritten": {"reply", map[string]string{"chat": "t1", "body": "b", "subject": "Urgent"}, "use send"},
	} {
		_, err := f.c.Prepare(ctx, tc.op, tc.args)
		var ce *connector.Error
		if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || !strings.Contains(ce.Message, tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if calls := f.jm.recorded(); len(calls) != 0 {
		t.Fatalf("prepare wrote %+v", calls)
	}
}

func compact(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApprovedSendAndReply(t *testing.T) {
	t.Parallel()
	const rpID, origin = "box.example.ts.net", "https://box.example.ts.net:8781"
	f := newFixture(t)
	seed(f.jm)
	f.link()
	key := approvaltest.New(t, rpID, origin)
	dir := t.TempDir()
	seen, err := approval.OpenSeen(filepath.Join(dir, "seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &connector.Server{Connector: f.c, Keys: approvaltest.Keys(t, key), Seen: seen, Log: f.env.Log}
	sock := filepath.Join(dir, "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, sock) }()
	t.Cleanup(func() { cancel(); <-done })
	client := connector.Dial(sock, 5*time.Second)
	eventually(t, "the socket serves", func() bool { _, err := client.Spec(context.Background()); return err == nil })

	approve := func(op string, args map[string]string) (connector.Result, approval.Approval) {
		t.Helper()
		p, err := client.Prepare(context.Background(), op, args)
		if err != nil {
			t.Fatal(err)
		}
		b, err := approval.Encode(approval.Record{
			ID: "p-" + op, Connector: "fastmail", Op: op, Args: p.Args, Display: p.Display,
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		a := key.Approve(b, time.Now())
		res, err := client.Execute(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		return res, a
	}

	res, a := approve("send", map[string]string{"to": "bob@example.com", "cc": "carol@example.org", "subject": "Plans", "body": "See you."})
	if res.Status != connector.Done || res.Ref == "" {
		t.Fatalf("send: %+v", res)
	}
	var ce *connector.Error
	if _, err := client.Execute(context.Background(), a); !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("replay: %v", err)
	}
	calls := f.jm.recorded()
	if len(calls) != 2 || calls[0].name != "Email/set" || calls[1].name != "EmailSubmission/set" {
		t.Fatalf("calls %+v", calls)
	}
	draft := calls[0].args["create"].(map[string]any)["draft"]
	if got, want := compact(t, draft), `{"bodyValues":{"body":{"value":"See you."}},"cc":[{"email":"carol@example.org"}],`+
		`"from":[{"email":"owner@fastmail.com","name":"Owner Name"}],"keywords":{"$draft":true,"$seen":true},`+
		`"mailboxIds":{"mb-drafts":true},"subject":"Plans","textBody":[{"partId":"body","type":"text/plain"}],`+
		`"to":[{"email":"bob@example.com"}]}`; got != want {
		t.Fatalf("draft\n got %s\nwant %s", got, want)
	}
	if got, want := compact(t, calls[1].args), `{"accountId":"u1","create":{"send":{"emailId":"#draft","identityId":"id-main"}},`+
		`"onSuccessUpdateEmail":{"#send":{"keywords/$draft":null,"mailboxIds/mb-drafts":null,"mailboxIds/mb-sent":true}}}`; got != want {
		t.Fatalf("submission\n got %s\nwant %s", got, want)
	}
	f.synced()
	if got := f.message(res.Ref); !got.FromMe || !strings.HasSuffix(got.Body, "\n\nSee you.") {
		t.Fatalf("the sent email is stored as %+v", got)
	}

	res, _ = approve("reply", map[string]string{"chat": "t2", "body": "Unsubscribe me."})
	if res.Status != connector.Done {
		t.Fatalf("reply: %+v", res)
	}
	calls = f.jm.recorded()
	draft = calls[2].args["create"].(map[string]any)["draft"]
	if got, want := compact(t, draft), `{"bodyValues":{"body":{"value":"Unsubscribe me."}},`+
		`"from":[{"email":"hi@owner.dev","name":"Owner"}],"inReplyTo":["e3@mail.example"],"keywords":{"$draft":true,"$seen":true},`+
		`"mailboxIds":{"mb-drafts":true},"references":["e3@mail.example"],"subject":"Re: Deals",`+
		`"textBody":[{"partId":"body","type":"text/plain"}],"to":[{"email":"deals@shop.example"}]}`; got != want {
		t.Fatalf("reply draft\n got %s\nwant %s", got, want)
	}
	if id := calls[3].args["create"].(map[string]any)["send"].(map[string]any)["identityId"]; id != "id-wild" {
		t.Fatalf("reply identity %v", id)
	}
	f.synced()
	if got := f.message(res.Ref); got.Chat != "t2" || got.ReplyTo != "e3" {
		t.Fatalf("the reply is stored as %+v", got)
	}

	// A reply to the owner's own email goes to its recipients and keeps the
	// whole References chain.
	p, err := f.c.Prepare(context.Background(), "reply", map[string]string{"chat": "t1", "seq": f.seq("e2"), "body": "x"})
	if err != nil || p.Args["to"] != "Alice@Example.com" {
		t.Fatalf("prepared %+v %v", p, err)
	}
	res = f.c.Execute(context.Background(), approval.Record{ID: "p3", Op: "reply", Args: p.Args})
	calls = f.jm.recorded()
	draft = calls[len(calls)-2].args["create"].(map[string]any)["draft"]
	if res.Status != connector.Done || compact(t, draft.(map[string]any)["references"]) != `["e1@mail.example","e2@mail.example"]` {
		t.Fatalf("reply to own email: %+v %s", res, compact(t, draft))
	}
}

func TestExecuteOutcomes(t *testing.T) {
	t.Parallel()
	rec := approval.Record{ID: "p1", Op: "send", Args: map[string]string{
		"from": "owner@fastmail.com", "to": "bob@example.com", "subject": "s", "body": "b",
	}}
	for mode, want := range map[string]connector.Status{
		"refuse":  connector.Failed,
		"error":   connector.Failed,
		"hangup":  connector.Unknown,
		"revoked": connector.Failed,
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.link()
			f.jm.set(func(j *fakeJMAP) {
				if mode == "revoked" {
					j.token = "fmu1-other"
				} else {
					j.submit = mode
				}
			})
			if res := f.c.Execute(context.Background(), rec); res.Status != want {
				t.Fatalf("result %+v, want %s", res, want)
			}
			calls := f.jm.recorded()
			switch mode {
			case "revoked":
				if len(calls) != 0 {
					t.Fatalf("calls %+v", calls)
				}
			case "hangup":
				if len(calls) != 2 {
					t.Fatalf("%d calls, want the one request", len(calls))
				}
			default:
				// The draft of an email Fastmail did not send is deleted.
				if len(calls) != 3 || compact(t, calls[2].args["destroy"]) != `["new-1"]` {
					t.Fatalf("calls %+v", calls)
				}
			}
		})
	}
}

func TestMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	e := textEmail("e1", "t1", alice, []address{owner}, "Topo", "route attached", now, "mb-inbox")
	e.Attachments = []bodyPart{
		{Type: "image/png", BlobID: "b-logo", Size: 4},
		{Type: "application/pdf", Name: "topo plan.pdf", BlobID: "b/topo", Size: 9},
	}
	f.jm.add(e)
	f.jm.set(func(j *fakeJMAP) { j.blobs = map[string]string{"b-logo": "\x89PNG", "b/topo": "%PDF-1.7\n"} })
	f.link()

	got, err := f.c.st.Media(ctx, "e1", 1)
	if err != nil || !reflect.DeepEqual(got, store.Media{Type: "application/pdf", Name: "topo plan.pdf", Size: 9, Ref: []byte("b/topo")}) {
		t.Fatalf("recorded %+v %v", got, err)
	}
	fetch := func(index int) (connector.MediaFile, string, error) {
		t.Helper()
		file, err := f.c.Media(ctx, "e1", index)
		if err != nil {
			return file, "", err
		}
		defer file.Body.Close()
		b, err := io.ReadAll(file.Body)
		return file, string(b), err
	}
	file, body, err := fetch(1)
	if err != nil || file.Type != "application/pdf" || file.Name != "topo plan.pdf" || file.Size != 9 || body != "%PDF-1.7\n" {
		t.Fatalf("fetched %+v %q %v", file, body, err)
	}

	// Stored before attachments were recorded: found on Fastmail by ID.
	if _, err := f.c.st.DB().Exec(`DELETE FROM media`); err != nil {
		t.Fatal(err)
	}
	if file, body, err := fetch(0); err != nil || file.Type != "image/png" || file.Size != 4 || body != "\x89PNG" {
		t.Fatalf("old message: %+v %q %v", file, body, err)
	}

	var ce *connector.Error
	f.jm.set(func(j *fakeJMAP) { delete(j.blobs, "b/topo") })
	if _, _, err := fetch(1); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("missing blob: %v", err)
	}
	if _, _, err := fetch(2); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("no such index: %v", err)
	}
}
