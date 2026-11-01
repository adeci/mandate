package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

// newSendFixture is a connected session whose phone has the test
// conversations, stored as a sync would.
func newSendFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, true)
	f.phone.setConvs(faridDM(), strangerDM(), family())
	f.activate()
	eventually(t, "the sync", func() bool {
		if len(chats(t, f)) != 3 {
			return false
		}
		f.c.in.mu.Lock()
		defer f.c.in.mu.Unlock()
		return !f.c.in.syncing
	})
	// Lookups go to the phone, as after a restart.
	f.c.in.reset()
	return f
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	f := newSendFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		chat string
		to   approval.Field
	}{
		{"1", approval.Field{Label: "To", Value: "Farid Haddad ((555) 123-0001)"}},
		// The name the stranger chose is not shown.
		{"2", approval.Field{Label: "To", Value: "(555) 999-0002 (not in your contacts)"}},
		{"3", approval.Field{Label: "Group", Value: "Family"}},
	} {
		args := map[string]string{"chat": tc.chat, "text": "hi"}
		p, err := f.c.Prepare(ctx, "send", args)
		if err != nil {
			t.Fatalf("%s: %v", tc.chat, err)
		}
		want := connector.Prepared{
			Args:      args,
			Display:   []approval.Field{tc.to, {Label: "Text", Value: "hi", Param: "text"}},
			Container: tc.chat,
		}
		if !reflect.DeepEqual(p, want) {
			t.Fatalf("%s:\n got %+v\nwant %+v", tc.chat, p, want)
		}
		again, err := f.c.Prepare(ctx, "send", p.Args)
		if err != nil || !reflect.DeepEqual(again, p) {
			t.Fatalf("%s: preparing its own output gave %+v, %v", tc.chat, again, err)
		}
	}

	for _, args := range []map[string]string{
		{"chat": "1", "text": " \n"},
		{"chat": "1", "text": "hi", "reply_to": "m1"},
		{"chat": "99", "text": "hi"},
	} {
		_, err := f.c.Prepare(ctx, "send", args)
		wantStatus(t, err, http.StatusBadRequest)
	}
	if _, err := f.c.Prepare(ctx, "delete", map[string]string{"chat": "1"}); !errors.Is(err, connector.ErrUnknownOp) {
		t.Fatalf("unknown op: %v", err)
	}
	// A stored chat the phone does not answer for cannot be shown.
	f.phone.setConvs()
	f.c.in.reset()
	_, err := f.c.Prepare(ctx, "send", map[string]string{"chat": "1", "text": "hi"})
	wantStatus(t, err, http.StatusServiceUnavailable)
}

func TestExecute(t *testing.T) {
	t.Parallel()
	f := newSendFixture(t)
	ctx := context.Background()
	f.cli().emit(&gmproto.Settings{SIMCards: []*gmproto.SIMCard{
		{SIMParticipant: &gmproto.SIMParticipant{ID: "1"}, SIMData: &gmproto.SIMData{SIMPayload: &gmproto.SIMPayload{Two: 2, SIMNumber: 1}}},
		{SIMParticipant: &gmproto.SIMParticipant{ID: "4"}, SIMData: &gmproto.SIMData{SIMPayload: &gmproto.SIMPayload{Two: 2, SIMNumber: 2}}},
	}})
	send := func(chat string) connector.Result {
		return f.c.Execute(ctx, approval.Record{Op: "send", Args: map[string]string{"chat": chat, "text": "on my way"}})
	}

	if res := send("3"); res.Status != connector.Done {
		t.Fatalf("send: %+v", res)
	}
	req := f.phone.sent[0]
	pl := req.GetMessagePayload()
	if req.GetConversationID() != "3" || pl.GetConversationID() != "3" || pl.GetParticipantID() != "4" ||
		req.GetSIMPayload().GetSIMNumber() != 2 || req.GetTmpID() == "" || pl.GetTmpID() != req.GetTmpID() ||
		len(pl.GetMessageInfo()) != 1 || pl.GetMessageInfo()[0].GetMessageContent().GetContent() != "on my way" {
		t.Fatalf("request %v", req)
	}

	for _, tc := range []struct {
		status gmproto.SendMessageResponse_Status
		err    error
		want   connector.Status
	}{
		{gmproto.SendMessageResponse_FAILURE_2, nil, connector.Unknown},
		{gmproto.SendMessageResponse_FAILURE_4, nil, connector.Failed},
		{0, libgm.ErrPhoneNotResponding, connector.Unknown},
	} {
		f.phone.mu.Lock()
		f.phone.sendResp, f.phone.sendErr = &gmproto.SendMessageResponse{Status: tc.status}, tc.err
		f.phone.mu.Unlock()
		if res := send("1"); res.Status != tc.want || res.Detail == "" {
			t.Errorf("%v %v: %+v, want %s", tc.status, tc.err, res, tc.want)
		}
	}

	// With the phone unreachable nothing is sent.
	f.cli().emit(&events.PhoneNotResponding{})
	if res := send("1"); res.Status != connector.Failed {
		t.Fatalf("send with the phone down: %+v", res)
	}
	if n := len(f.phone.sent); n != 4 {
		t.Fatalf("%d requests sent", n)
	}
}
