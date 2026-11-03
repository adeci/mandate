package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

func (h *harness) accessChallenge(t *testing.T, req accessChallengeRequest) (int, accessChallengeResponse, []byte) {
	t.Helper()
	status, body := h.post(t, "/access/challenge", req)
	var res accessChallengeResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
	}
	return status, res, body
}

func (h *harness) accessApprove(t *testing.T, session string, res accessChallengeResponse) (int, []byte) {
	t.Helper()
	c, err := base64.RawURLEncoding.DecodeString(res.Options.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	return h.call(t, http.MethodPost, "/access/approve", session, accessApproveRequest{Nonce: res.Nonce, Assertion: h.key.Assert(c)})
}

// sign signs req with the harness key and fails the test unless it is
// stored.
func (h *harness) sign(t *testing.T, req accessChallengeRequest) (accessApproveResponse, accessChallengeResponse) {
	t.Helper()
	status, res, body := h.accessChallenge(t, req)
	if status != http.StatusOK {
		t.Fatalf("challenge %+v: %d %s", req, status, body)
	}
	status, body = h.accessApprove(t, h.session, res)
	var out accessApproveResponse
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("approve %+v: %d %s", req, status, body)
	}
	return out, res
}

// newTokenHash is the hash of a token as the browser makes it; the console
// only ever sees that.
func newTokenHash() string {
	sum := sha256.Sum256([]byte(rand.Text()))
	return hex.EncodeToString(sum[:])
}

func (h *harness) agent(t *testing.T, name string) string {
	t.Helper()
	hash := newTokenHash()
	out, _ := h.sign(t, accessChallengeRequest{Kind: approval.KindAgent, Name: name, TokenSHA256: hash})
	return out.ID
}

func (h *harness) records(t *testing.T) (agents []approval.Agent, grants []approval.Grant) {
	t.Helper()
	recs, err := h.api.Access(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range recs {
		switch s.Kind {
		case approval.KindAgent:
			a, err := h.srv.verifier.VerifyAgent(s)
			if err != nil {
				t.Fatal(err)
			}
			agents = append(agents, a)
		case approval.KindGrant:
			g, err := h.srv.verifier.VerifyGrant(s)
			if err != nil {
				t.Fatal(err)
			}
			grants = append(grants, g)
		}
	}
	return agents, grants
}

func (h *harness) requestAccess(t *testing.T, req proposal.AccessRequest) proposal.RequestRef {
	t.Helper()
	ref, err := h.api.RequestAccess(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// named asks agent for chats of the fake connector, one term matching
// nothing.
func named(agent string) proposal.AccessRequest {
	return proposal.AccessRequest{
		AgentID: agent, Connector: "fake",
		Terms:     []string{"family", "Mom", "lawyer"},
		Chats:     []proposal.Chat{{ID: "c1", Name: "Guild / family"}, {ID: "d1", Name: "~Mom"}},
		Unmatched: []string{"lawyer"},
		For:       2 * time.Hour, Horizon: 7 * 24 * time.Hour, Reason: "summarise the week",
	}
}

// forwarding forwards raw reads, so it keeps no chats.
type forwarding struct{ *fake }

func (forwarding) Forward(w http.ResponseWriter, _ *http.Request, _ string) {
	w.WriteHeader(http.StatusNoContent)
}

func (h *harness) accessStatus(t *testing.T, id string) proposal.AccessStatus {
	t.Helper()
	st, err := h.api.AccessStatus(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAgentTokenNeverReachesConsole(t *testing.T) {
	h := start(t)
	hash := newTokenHash()
	for _, bad := range []string{"", strings.ToUpper(hash), hash[:62]} {
		if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindAgent, Name: "bot", TokenSHA256: bad}); status != http.StatusBadRequest {
			t.Errorf("token_sha256 %q: %d", bad, status)
		}
	}
	status, res, body := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindAgent, Name: "Grok Bot", TokenSHA256: hash})
	if status != http.StatusOK {
		t.Fatalf("challenge: %d %s", status, body)
	}
	if status, _ := h.accessApprove(t, h.login(t, h.key), res); status != http.StatusForbidden {
		t.Fatalf("approve from another session: %d", status)
	}
	status, body = h.accessApprove(t, h.session, res)
	if status != http.StatusOK || bytes.Contains(body, []byte("token")) {
		t.Fatalf("approve: %d %s", status, body)
	}
	if status, _ := h.accessApprove(t, h.session, res); status != http.StatusConflict {
		t.Fatalf("approve again: %d", status)
	}
	agents, _ := h.records(t)
	if len(agents) != 1 || agents[0].Name != "Grok Bot" || agents[0].TokenSHA256 != hash {
		t.Fatalf("agents = %+v", agents)
	}
}

func TestAccessRequestLifecycle(t *testing.T) {
	notes := make(chan *http.Request, 4)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		notes <- r
	}))
	t.Cleanup(ntfy.Close)
	h := startWith(t, func(cfg *Config, _ string) { cfg.NtfyURL, cfg.NtfyToken = ntfy.URL, "tk" })
	agent := h.agent(t, "Grok Bot")

	ref := h.requestAccess(t, named(agent))
	note := <-notes
	text, _ := io.ReadAll(note.Body)
	if string(text) != "Grok Bot asks to read fake" || note.Header.Get("Title") != "Grok Bot" || note.Header.Get("Priority") != "high" ||
		note.Header.Get("Click") != ref.URL || note.Header.Get("Authorization") != "Bearer tk" {
		t.Fatalf("notification %q %v", text, note.Header)
	}
	if st := h.accessStatus(t, ref.ID); st.Status != proposal.AccessPending || st.AgentID != agent {
		t.Fatalf("status = %+v", st)
	}
	_, page := h.page(t, "/")
	for _, want := range []string{
		`id="request-` + ref.ID + `"`, `<strong class="content">Grok Bot</strong> wants to read`,
		`<input type="checkbox" value="t1"> <span class="content">cats</span>`, `~Mom`, `no visible chat matched "lawyer"`,
		"summarise the week", `data-span="2 hours" disabled>Approve</button>`, "tick the chats to grant",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("Now lacks %s", want)
		}
	}
	if strings.Contains(page, `checked> <span class="content">`) {
		t.Error("a requested chat starts ticked")
	}

	for name, req := range map[string]accessChallengeRequest{
		"no choice":     {},
		"no chats":      {Chats: []string{}},
		"another chat":  {Chats: []string{"c2"}},
		"longer":        {Chats: []string{"c1"}, For: "4h"},
		"more history":  {Chats: []string{"c1"}, Horizon: "720h"},
		"every message": {Chats: []string{"c1"}, Horizon: "0s"},
	} {
		req.Kind, req.Request = approval.KindGrant, ref.ID
		if status, _, _ := h.accessChallenge(t, req); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	granted, res := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"c1", "t1"}, For: "1h", Horizon: "24h"})
	if !strings.Contains(string(res.Summary), "family (c1), cats (t1) (chats created later are not included)") {
		t.Errorf("summary %q", res.Summary)
	}
	st := h.accessStatus(t, ref.ID)
	_, grants := h.records(t)
	if st.Status != proposal.AccessGranted || st.GrantID != granted.ID || len(grants) != 1 {
		t.Fatalf("status = %+v, grants = %+v", st, grants)
	}
	g := grants[0]
	if g.Agent != agent || !slices.Equal(g.Chats, []string{"c1", "t1"}) || g.Horizon != 24*time.Hour ||
		g.Until.Sub(g.From) != time.Hour || !st.Until.Equal(g.Until) {
		t.Fatalf("grant = %+v", g)
	}

	p, err := h.api.Submit(context.Background(), proposal.Request{
		Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": "secret"}, AgentID: agent, AgentName: "Grok Bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	note = <-notes
	if text, _ := io.ReadAll(note.Body); string(text) != "Grok Bot proposes a fake send" || note.Header.Get("Click") != p.URL ||
		note.Header.Get("Priority") != "" {
		t.Fatalf("proposal notification %q %v", text, note.Header)
	}
	if _, page := h.page(t, "/r/"+p.ID); !strings.Contains(page, "from Grok Bot") {
		t.Fatal("proposal page doesn't name the agent")
	}
	if _, err := h.api.Get(context.Background(), p.ID, agent); err != nil {
		t.Fatal(err)
	}
	var e *connector.Error
	if _, err := h.api.Get(context.Background(), p.ID, "other"); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("another agent's proposal: %v", err)
	}

	denied := h.requestAccess(t, named(agent))
	<-notes
	waited := make(chan proposal.AccessStatus)
	go func() {
		st, err := h.api.AccessStatus(context.Background(), denied.ID, 30*time.Second)
		if err != nil {
			t.Error(err)
		}
		waited <- st
	}()
	time.Sleep(100 * time.Millisecond)
	if status, _ := h.post(t, "/access/requests/"+denied.ID+"/deny", nil); status != http.StatusSeeOther {
		t.Fatalf("deny: %d", status)
	}
	select {
	case st := <-waited:
		if st.Status != proposal.AccessDenied {
			t.Fatalf("waited for %+v", st)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long poll didn't end on the decision")
	}

	expired := h.requestAccess(t, named(agent))
	<-notes
	h.clock.advance(accessRequestTTL)
	if st := h.accessStatus(t, expired.ID); st.Status != proposal.AccessExpired {
		t.Fatalf("after an hour: %+v", st)
	}
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindGrant, Request: expired.ID, Chats: []string{"c1"}}); status != http.StatusConflict {
		t.Fatalf("approve expired: %d", status)
	}
}

func TestAccessRequestLimits(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	busy, other := h.agent(t, "busy"), h.agent(t, "other")
	// tooMany asks for the quota and then requests, as the gateway does; both
	// must agree.
	tooMany := func(agent string) bool {
		quota := h.api.AccessQuota(ctx, agent)
		_, err := h.api.RequestAccess(ctx, named(agent))
		for _, err := range []error{quota, err} {
			var e *connector.Error
			if err != nil && (!errors.As(err, &e) || e.Status != http.StatusTooManyRequests) {
				t.Fatal(err)
			}
		}
		if (quota == nil) != (err == nil) {
			t.Fatalf("quota %v, request %v", quota, err)
		}
		return err != nil
	}
	var waiting []string
	for range maxWaitingRequests {
		waiting = append(waiting, h.requestAccess(t, named(busy)).ID)
	}
	if !tooMany(busy) || tooMany(other) {
		t.Fatal("the waiting cap isn't per agent")
	}
	for i := maxWaitingRequests; i < maxHourlyRequests; i++ {
		if status, _ := h.post(t, "/access/requests/"+waiting[0]+"/deny", nil); status != http.StatusSeeOther {
			t.Fatalf("deny: %d", status)
		}
		waiting = append(waiting[1:], h.requestAccess(t, named(busy)).ID)
	}
	for _, id := range waiting {
		h.post(t, "/access/requests/"+id+"/deny", nil)
	}
	if !tooMany(busy) {
		t.Fatal("the hourly cap doesn't hold")
	}
	h.clock.advance(time.Hour)
	if tooMany(busy) {
		t.Fatal("the hourly cap doesn't roll")
	}
	var e *connector.Error
	if err := h.api.AccessQuota(ctx, "nobody"); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("quota of an unknown agent: %v", err)
	}
}

func TestAccessTermsCheckedAsGiven(t *testing.T) {
	long := strings.Repeat("x", maxTerm+1)
	base := proposal.AccessRequest{For: time.Hour, Reason: "r"}
	for name, tc := range map[string]struct {
		terms, unmatched []string
		chats            []proposal.Chat
		ok               bool
	}{
		"matched":               {[]string{"fam"}, nil, []proposal.Chat{{ID: "c1"}}, true},
		"unmatched":             {[]string{"fam"}, []string{"fam"}, nil, true},
		"long and matched":      {[]string{long}, nil, []proposal.Chat{{ID: "c1"}}, false},
		"long and unmatched":    {[]string{long}, []string{long}, nil, false},
		"duplicate":             {[]string{"fam", "fam"}, nil, []proposal.Chat{{ID: "c1"}}, false},
		"untrimmed":             {[]string{" fam"}, nil, []proposal.Chat{{ID: "c1"}}, false},
		"unmatched not given":   {[]string{"fam"}, []string{"work"}, []proposal.Chat{{ID: "c1"}}, false},
		"chats without terms":   {nil, nil, []proposal.Chat{{ID: "c1"}}, false},
		"terms without results": {[]string{"fam"}, nil, nil, false},
		"too many chats":        {[]string{"fam"}, nil, slices.Repeat([]proposal.Chat{{ID: "c1"}}, chatsPerTerm+1), false},
	} {
		req := base
		req.Terms, req.Unmatched, req.Chats = tc.terms, tc.unmatched, tc.chats
		if err := checkAccessRequest(req); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestEveryChatRequestNarrows(t *testing.T) {
	h := start(t)
	agent := h.agent(t, "bot")
	ref := h.requestAccess(t, proposal.AccessRequest{AgentID: agent, Connector: "fake", Horizon: 24 * time.Hour, Reason: "find the invite"})
	_, res := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"c2"}})
	_, grants := h.records(t)
	if len(grants) != 1 || !slices.Equal(grants[0].Chats, []string{"c2"}) || !strings.Contains(string(res.Summary), "Work (c2)") {
		t.Fatalf("grants %+v, summary %q", grants, res.Summary)
	}
}

func TestRequestShowsBoundedTree(t *testing.T) {
	h := start(t)
	agent := h.agent(t, "bot")
	ref := h.requestAccess(t, proposal.AccessRequest{
		AgentID: agent, Connector: "fake", Terms: []string{"Work"}, Chats: []proposal.Chat{{ID: "c2", Name: "Work"}},
		For: time.Hour, Reason: "r",
	})
	_, page := h.page(t, "/")
	if more := len(workThreads) - (treeNodes - 1); !strings.Contains(page, fmt.Sprintf("and %d more under it", more)) {
		t.Fatal("the request doesn't say how many chats it leaves out")
	}
	last := workThreads[len(workThreads)-1].ID
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{last}}); status != http.StatusBadRequest {
		t.Fatalf("granting a chat left out: %d", status)
	}
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"c2", workThreads[0].ID}})
}

func TestDeletedAgentsProposalsEnd(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent := h.agent(t, "bot")
	submit := func() (proposal.Proposal, error) {
		return h.api.Submit(ctx, proposal.Request{
			Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": "hi"}, AgentID: agent, AgentName: "bot",
		})
	}
	waiting, err := submit()
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, "/access/agents/"+agent+"/delete", nil); status != http.StatusSeeOther {
		t.Fatalf("delete: %d", status)
	}
	if p, err := h.api.Get(ctx, waiting.ID, agent); err != nil || p.Status != proposal.Rejected {
		t.Fatalf("waiting proposal after delete: %+v %v", p, err)
	}
	if _, err := submit(); err == nil {
		t.Fatal("a deleted agent proposed")
	}
	// One that slipped in as the agent was deleted can't be approved.
	_, err = h.srv.db.sql.ExecContext(ctx, `UPDATE proposals SET status = 'pending', decided_at = NULL WHERE id = ?`, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, "/r/"+waiting.ID+"/challenge", challengeRequest{}); status != http.StatusConflict {
		t.Fatalf("challenge: %d", status)
	}
	if _, page := h.page(t, "/r/"+waiting.ID); !strings.Contains(page, "was deleted") || strings.Contains(page, "data-review") {
		t.Fatal("the request page offers approval")
	}
}

func TestForwardOnlyConnector(t *testing.T) {
	h := startWith(t, func(cfg *Config, dir string) {
		web, old := filepath.Join(dir, "web.sock"), filepath.Join(dir, "old.sock")
		serveSocket(t, web, (&connector.Server{Connector: forwarding{&fake{name: "web"}}, Log: discard}).Handler())
		// old neither forwards nor lists chats.
		serveSocket(t, old, (&connector.Server{Connector: &fake{name: "old"}, Log: discard}).Handler())
		cfg.Connectors["web"], cfg.Connectors["old"] = web, old
	})
	if status, _ := h.call(t, http.MethodGet, "/access/chats?connector=old", h.session, nil); status == http.StatusOK {
		t.Fatal("a connector without /containers listed no chats")
	}
	agent := h.agent(t, "bot")
	for name, req := range map[string]accessChallengeRequest{
		"horizon": {Horizon: "24h", For: "1h"},
		"chats":   {Chats: []string{"x"}, For: "1h"},
	} {
		req.Kind, req.Agent, req.Connector = approval.KindGrant, agent, "web"
		if status, _, _ := h.accessChallenge(t, req); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	_, res := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: agent, Connector: "web", For: "1h"})
	if !strings.Contains(string(res.Summary), "every allowlisted read on web") {
		t.Errorf("summary %q", res.Summary)
	}
	if _, err := h.api.RequestAccess(context.Background(), proposal.AccessRequest{
		AgentID: agent, Connector: "web", Terms: []string{"x"}, Chats: []proposal.Chat{{ID: "x"}}, Reason: "r",
	}); err == nil {
		t.Fatal("a request naming chats on a forward-only connector was taken")
	}
	ref := h.requestAccess(t, proposal.AccessRequest{AgentID: agent, Connector: "web", Horizon: 7 * 24 * time.Hour, Reason: "r"})
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID})
	_, grants := h.records(t)
	for _, g := range grants {
		if g.Chats != nil || g.Horizon != 0 {
			t.Fatalf("forward-only grant %+v", g)
		}
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v", grants)
	}
}

func TestAccessRecordsLeaveWhenEnded(t *testing.T) {
	h := start(t)
	kept, gone := h.agent(t, "kept"), h.agent(t, "gone")
	routine, _ := h.sign(t, accessChallengeRequest{
		Kind: approval.KindGrant, Agent: kept, Connector: "fake", Horizon: "24h",
		Schedule: &scheduleRequest{Days: []time.Weekday{time.Monday}, Start: "07:00", Length: "1h", Zone: "Europe/Berlin"},
	})
	revoked, _ := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: kept, Connector: "fake", For: "1h"})
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: gone, Connector: "fake", For: "1h"})
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{
		Kind: approval.KindGrant, Agent: kept, Connector: "fake",
		Schedule: &scheduleRequest{Start: "07:00", Length: "1h", Zone: "Local"},
	}); status != http.StatusBadRequest {
		t.Fatalf("schedule without a zone: %d", status)
	}

	if status, _ := h.post(t, "/access/grants/"+revoked.ID+"/close", nil); status != http.StatusSeeOther {
		t.Fatalf("close: %d", status)
	}
	if status, _ := h.post(t, "/access/agents/"+gone+"/delete", nil); status != http.StatusSeeOther {
		t.Fatalf("delete: %d", status)
	}
	agents, grants := h.records(t)
	if len(agents) != 1 || agents[0].ID != kept || len(grants) != 1 || grants[0].ID != routine.ID {
		t.Fatalf("live: agents %+v, grants %+v", agents, grants)
	}
	var signed int
	err := h.srv.db.sql.QueryRowContext(context.Background(), `SELECT (SELECT count(signed) FROM agents) + (SELECT count(signed) FROM grants)`).Scan(&signed)
	if err != nil || signed != 2 {
		t.Fatalf("signatures kept: %d %v", signed, err)
	}
	if _, page := h.page(t, "/"); !strings.Contains(page, "Mon at 07:00 for 1 hour (Europe/Berlin)") {
		t.Fatal("Now doesn't describe the routine")
	}
	if _, page := h.page(t, "/agents"); !strings.Contains(page, "1 ended") || !strings.Contains(page, ">Pause</button>") || strings.Contains(page, ">gone<") {
		t.Fatal("Agents doesn't list the routine and the ended window, or lists the deleted agent")
	}
}

func TestReadsAndChatPicker(t *testing.T) {
	h := start(t)
	status, body := h.call(t, http.MethodGet, "/access/chats?connector=fake&q=FAM", h.session, nil)
	var chats chatsResponse
	if err := json.Unmarshal(body, &chats); status != http.StatusOK || err != nil ||
		len(chats.Chats) != 1 || chats.Chats[0].ID != "c1" || chats.Chats[0].Name != "family" || chats.Chats[0].Label != "family" {
		t.Fatalf("chats: %d %s", status, body)
	}
	status, body = h.call(t, http.MethodGet, "/access/chats/tree?connector=fake&id=g", h.session, nil)
	var tree chatNode
	if err := json.Unmarshal(body, &tree); status != http.StatusOK || err != nil || len(tree.Children) != 1 ||
		tree.Children[0].ID != "c1" || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].ID != "t1" {
		t.Fatalf("tree: %d %s", status, body)
	}
	agent := h.agent(t, "bot")
	// Today starts at midnight; reads an hour back must fall within it.
	if now := h.clock.now(); now.Sub(startOfDay(now)) < 2*time.Hour {
		h.clock.advance(2 * time.Hour)
	}
	for i, at := range []time.Duration{0, 5 * time.Minute, 30 * time.Minute} {
		err := h.api.Read(context.Background(), proposal.ReadEvent{
			AgentID: agent, Connector: "fake", Verb: "messages", Chat: "c1", ChatName: "family", Grants: []string{"g1"},
			Count: 3, Dropped: i, At: h.clock.now().Add(-time.Hour + at),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, page := h.page(t, "/")
	if strings.Count(page, "bot read family on fake") != 2 || !strings.Contains(page, "· 6 messages ·") || !strings.Contains(page, "1 read not logged") {
		t.Fatal("Today doesn't group the reads into sessions")
	}
	ref := h.requestAccess(t, named(agent))
	h.post(t, "/access/requests/"+ref.ID+"/deny", nil)
	ctx := context.Background()
	if err := h.srv.db.sweep(ctx, h.clock.now().Add(accessKept+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reads, _, err := h.srv.db.readSessions(ctx, time.Time{}, h.clock.now().Add(accessKept+time.Minute), h.clock.now().Add(accessKept+time.Minute), 10); err != nil || len(reads) != 0 {
		t.Fatalf("reads after 30 days: %v %v", reads, err)
	}
	if _, err := h.srv.db.accessRequest(ctx, ref.ID, h.clock.now()); !errors.Is(err, errNotFound) {
		t.Fatalf("decided request after 30 days: %v", err)
	}
}

func (h *harness) nowJSON(t *testing.T) nowData {
	t.Helper()
	status, body := h.call(t, http.MethodGet, "/now.json", h.session, nil)
	var d nowData
	if status != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("now.json: %d %s", status, body)
	}
	return d
}

func ids(gs []grantView) []string {
	out := []string{}
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

func TestPreTicking(t *testing.T) {
	h := start(t)
	agents := []string{h.agent(t, "bot"), h.agent(t, "other")}
	i := 0
	for term, chat := range map[string]proposal.Chat{
		"Dad":      {ID: "d2", Name: "Dad"},
		"Mom":      {ID: "d1", Name: "~Mom"},
		"Work":     {ID: "c2", Name: "Work"},
		"stranger": {ID: "d3"},
	} {
		h.requestAccess(t, proposal.AccessRequest{
			AgentID: agents[i%2], Connector: "fake", Terms: []string{term}, Chats: []proposal.Chat{chat}, For: time.Hour, Reason: "r",
		})
		i++
	}
	ticked := map[string]string{}
	for _, c := range h.nowJSON(t).Waiting {
		for _, n := range c.Chats {
			if n.Ticked {
				ticked[n.ID] = c.Approve
			}
		}
	}
	// Only the DM under a name the owner gave it, the one chat that matched;
	// a DM with no name may have been named by anyone.
	if len(ticked) != 1 || ticked["d2"] != "Approve · 1 hour · Dad · all history" {
		t.Fatalf("ticked = %v", ticked)
	}
}

func TestExtendRequest(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent := h.agent(t, "bot")
	extend := proposal.AccessRequest{AgentID: agent, Connector: "fake", Extend: true, For: 30 * time.Minute, Reason: "still reading"}
	var e *connector.Error
	if _, err := h.api.RequestAccess(ctx, extend); !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Fatalf("extend with nothing open: %v", err)
	}
	first, _ := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: agent, Connector: "fake", Chats: []string{"c2"}, For: "1h", Horizon: "24h"})
	_, grants := h.records(t)
	until := grants[0].Until

	ref := h.requestAccess(t, extend)
	if w := h.nowJSON(t).Waiting; len(w) != 1 || w[0].Kind != "extend" || w[0].Extends == nil || w[0].Extends.ID != first.ID {
		t.Fatalf("waiting = %+v", w)
	}
	got, _ := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID})
	_, grants = h.records(t)
	if len(grants) != 1 || grants[0].ID != got.ID || !slices.Equal(grants[0].Chats, []string{"c2"}) ||
		grants[0].Horizon != 24*time.Hour || !grants[0].Until.Equal(until.Add(30*time.Minute)) {
		t.Fatalf("after extending: %+v", grants)
	}

	// The owner's +30m does the same from Now.
	more, _ := h.sign(t, accessChallengeRequest{Kind: kindExtend, Grant: got.ID, For: "30m"})
	_, grants = h.records(t)
	if len(grants) != 1 || grants[0].ID != more.ID || !grants[0].Until.Equal(until.Add(time.Hour)) {
		t.Fatalf("after +30m: %+v", grants)
	}
}

func TestRoutinePauseResume(t *testing.T) {
	h := start(t)
	agent := h.agent(t, "bot")
	ref := h.requestAccess(t, proposal.AccessRequest{
		AgentID: agent, Connector: "fake", Terms: []string{"Work"}, Chats: []proposal.Chat{{ID: "c2", Name: "Work"}},
		Horizon:  24 * time.Hour,
		Schedule: &proposal.RequestSchedule{Days: []time.Weekday{1, 2, 3, 4, 5}, Start: "07:00", Length: 20 * time.Minute},
		Reason:   "morning summary",
	})
	if w := h.nowJSON(t).Waiting; len(w) != 1 || w[0].Kind != "routine" || w[0].When != "Weekdays 07:00 · 20 min" {
		t.Fatalf("waiting = %+v", w)
	}
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"c2"}}); status != http.StatusBadRequest {
		t.Fatalf("a routine without the owner's zone: %d", status)
	}
	routine, _ := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"c2"}, Zone: "America/New_York"})
	_, grants := h.records(t)
	if sc := grants[0].Schedule; len(grants) != 1 || sc == nil || sc.Zone != "America/New_York" || sc.Start != "07:00" || sc.Length != 20*time.Minute {
		t.Fatalf("routine = %+v", grants)
	}

	if status, _ := h.post(t, "/access/grants/"+routine.ID+"/close", nil); status != http.StatusSeeOther {
		t.Fatalf("pause: %d", status)
	}
	if d := h.nowJSON(t); len(d.Routines) != 0 || !slices.Equal(ids(d.Stopped), []string{routine.ID}) {
		t.Fatalf("paused: routines %v, stopped %v", ids(d.Routines), ids(d.Stopped))
	}
	resumed, _ := h.sign(t, accessChallengeRequest{Kind: kindResume, Grant: routine.ID})
	_, grants = h.records(t)
	if resumed.ID == routine.ID || len(grants) != 1 || grants[0].ID != resumed.ID || grants[0].Schedule.Zone != "America/New_York" {
		t.Fatalf("resumed %s: %+v", resumed.ID, grants)
	}
	if d := h.nowJSON(t); !slices.Equal(ids(d.Routines), []string{resumed.ID}) || len(d.Stopped) != 0 {
		t.Fatalf("resumed: routines %v, stopped %v", ids(d.Routines), ids(d.Stopped))
	}
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: kindResume, Grant: routine.ID}); status != http.StatusNotFound {
		t.Fatalf("resuming twice: %d", status)
	}
}

func TestStopAll(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent := h.agent(t, "bot")
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: agent, Connector: "fake", For: "1h"})
	h.requestAccess(t, named(agent))
	h.clock.advance(2 * time.Hour)
	// Only what is still open, due, or waiting counts.
	h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Agent: agent, Connector: "fake", For: "1h"})
	routine, _ := h.sign(t, accessChallengeRequest{
		Kind: approval.KindGrant, Agent: agent, Connector: "fake",
		Schedule: &scheduleRequest{Start: "07:00", Length: "1h", Zone: "UTC"},
	})
	ref := h.requestAccess(t, named(agent))
	p, err := h.api.Submit(ctx, proposal.Request{
		Connector: "fake", Op: "send", Args: map[string]string{"to": "Farid", "text": "hi"}, AgentID: agent, AgentName: "bot",
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := h.do(t, http.MethodPost, "/access/stop-all", h.session, nil)
	resp.Body.Close()
	where := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("stop all: %d", resp.StatusCode)
	}
	if st := h.accessStatus(t, ref.ID); st.Status != proposal.AccessDenied {
		t.Fatalf("request after stop all: %+v", st)
	}
	if got, err := h.api.Get(ctx, p.ID, agent); err != nil || got.Status != proposal.Pending {
		t.Fatalf("proposal after stop all: %+v %v", got, err)
	}
	d := h.nowJSON(t)
	if len(d.Waiting) != 1 || d.Waiting[0].Kind != "send" || len(d.Open) != 0 || !slices.Equal(ids(d.Stopped), []string{routine.ID}) {
		t.Fatalf("now after stop all: %+v", d)
	}
	const notice = "Stopped: 2 grants closed, 1 request denied"
	if _, page := h.page(t, where); !strings.Contains(page, notice) {
		t.Errorf("%s lacks %q", where, notice)
	}
	if _, page := h.page(t, "/?stopped=99"); strings.Contains(page, "Stopped:") {
		t.Error("a notice for a Stop all that never happened")
	}
	h.clock.advance(noticeFor)
	if _, page := h.page(t, where); strings.Contains(page, "Stopped:") {
		t.Error("an old Stop all's notice still shows")
	}
}

func TestSeen(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent := h.agent(t, "bot")
	first := h.clock.now().UTC().Truncate(time.Millisecond)
	for _, at := range []time.Time{first, first.Add(time.Minute)} {
		if err := h.api.Seen(ctx, proposal.Seen{AgentID: agent, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	status, body := h.call(t, http.MethodGet, "/agents.json", h.session, nil)
	var d agentsData
	if status != http.StatusOK || json.Unmarshal(body, &d) != nil || len(d.Agents) != 1 ||
		!d.Agents[0].FirstSeen.Equal(first) || !d.Agents[0].LastSeen.Equal(first.Add(time.Minute)) {
		t.Fatalf("agents.json: %d %s", status, body)
	}
}

// An approval under way ends with the grant it would extend, with Stop all,
// and with its agent.
func TestApprovalsUnderWayEnd(t *testing.T) {
	h := start(t)
	agent := h.agent(t, "bot")
	challenge := func(req accessChallengeRequest) accessChallengeResponse {
		t.Helper()
		status, res, body := h.accessChallenge(t, req)
		if status != http.StatusOK {
			t.Fatalf("challenge %+v: %d %s", req, status, body)
		}
		return res
	}
	refused := func(what string, res accessChallengeResponse) {
		t.Helper()
		if status, body := h.accessApprove(t, h.session, res); status != http.StatusConflict {
			t.Errorf("%s: %d %s", what, status, body)
		}
	}
	closeGrant := func(id string) {
		t.Helper()
		if status, _ := h.post(t, "/access/grants/"+id+"/close", nil); status != http.StatusSeeOther {
			t.Fatalf("close: %d", status)
		}
	}
	window := accessChallengeRequest{Kind: approval.KindGrant, Agent: agent, Connector: "fake", Chats: []string{"c2"}, For: "1h"}

	g, _ := h.sign(t, window)
	more := challenge(accessChallengeRequest{Kind: kindExtend, Grant: g.ID, For: "30m"})
	closeGrant(g.ID)
	refused("+30m after Close", more)

	g, _ = h.sign(t, window)
	ref := h.requestAccess(t, proposal.AccessRequest{AgentID: agent, Connector: "fake", Extend: true, For: 30 * time.Minute, Reason: "more"})
	asked := challenge(accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID})
	closeGrant(g.ID)
	refused("the agent's extension after Close", asked)

	g, _ = h.sign(t, window)
	routine, _ := h.sign(t, accessChallengeRequest{
		Kind: approval.KindGrant, Agent: agent, Connector: "fake",
		Schedule: &scheduleRequest{Start: "07:00", Length: "1h", Zone: "UTC"},
	})
	closeGrant(routine.ID)
	read := h.requestAccess(t, named(agent))
	underway := map[string]accessChallengeResponse{
		"a new grant": challenge(window),
		"a request":   challenge(accessChallengeRequest{Kind: approval.KindGrant, Request: read.ID, Chats: []string{"c1"}}),
		"+30m":        challenge(accessChallengeRequest{Kind: kindExtend, Grant: g.ID, For: "30m"}),
		"a resume":    challenge(accessChallengeRequest{Kind: kindResume, Grant: routine.ID}),
	}
	if status, _ := h.post(t, "/access/stop-all", nil); status != http.StatusSeeOther {
		t.Fatalf("stop all: %d", status)
	}
	for what, res := range underway {
		refused(what+" after Stop all", res)
	}

	pending := challenge(window)
	if status, _ := h.post(t, "/access/agents/"+agent+"/delete", nil); status != http.StatusSeeOther {
		t.Fatalf("delete: %d", status)
	}
	refused("a grant after its agent was deleted", pending)
	if _, grants := h.records(t); len(grants) != 0 {
		t.Fatalf("grants = %+v", grants)
	}
}

// Today shows the newest read sessions of a day however many came before
// them, and continues with the earlier ones within the day.
func TestTodayShowsNewestSessions(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	agent := h.agent(t, "bot")
	day := startOfDay(h.clock.now())
	if h.clock.now().Sub(day) < 3*time.Hour {
		h.clock.advance(3 * time.Hour)
	}
	tx, err := h.srv.db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(at time.Time, verb, chat string) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reads (at, agent_id, connector, verb, chat, chat_name, grants, count, dropped)
			VALUES (?, ?, 'fake', ?, ?, ?, '[]', 1, 0)`, ms(at), agent, verb, chat, chat); err != nil {
			t.Fatal(err)
		}
	}
	noise := h.clock.now().Add(-2 * time.Hour)
	for i := range 20000 {
		insert(noise.Add(time.Duration(i)*time.Millisecond), "chats", "")
	}
	// More sessions than a page holds, one chat each.
	for i := range maxSessions + 5 {
		insert(noise.Add(time.Hour+time.Duration(i)*time.Millisecond), "messages", fmt.Sprintf("chat %d", i))
	}
	insert(h.clock.now().Add(-time.Minute), "messages", "Dad")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	d := h.nowJSON(t)
	if len(d.Today) == 0 || d.Today[0].Text != "bot read Dad on fake · 1 message" || d.Hidden != 7 || d.More == "" {
		t.Fatalf("today: %d lines, hidden %d, more %q", len(d.Today), d.Hidden, d.More)
	}
	status, body := h.call(t, http.MethodGet, strings.Replace(d.More, "/?", "/now.json?", 1), h.session, nil)
	var rest nowData
	if status != http.StatusOK || json.Unmarshal(body, &rest) != nil {
		t.Fatalf("more: %d %s", status, body)
	}
	if len(rest.Today) != 7 || rest.Hidden != 0 || rest.Today[6].Text != "bot listed fake chats" {
		t.Fatalf("earlier sessions: %+v", rest.Today)
	}
}

// When a request's terms matched no chat, the owner may pick chats instead,
// and the passkey summary names them.
func TestPickWhenNothingMatched(t *testing.T) {
	h := start(t)
	agent := h.agent(t, "bot")
	ref := h.requestAccess(t, proposal.AccessRequest{
		AgentID: agent, Connector: "fake", Terms: []string{"lawyer"}, Unmatched: []string{"lawyer"}, For: time.Hour, Reason: "r",
	})
	if w := h.nowJSON(t).Waiting; len(w) != 1 || !w[0].Pick || !w[0].CanApprove || w[0].Ready {
		t.Fatalf("waiting = %+v", w)
	}
	if status, _, _ := h.accessChallenge(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID}); status != http.StatusBadRequest {
		t.Fatalf("approving with no chats picked: %d", status)
	}
	_, res := h.sign(t, accessChallengeRequest{Kind: approval.KindGrant, Request: ref.ID, Chats: []string{"d2"}})
	if !strings.Contains(string(res.Summary), "Dad (d2)") {
		t.Errorf("summary %q", res.Summary)
	}
	if _, grants := h.records(t); len(grants) != 1 || !slices.Equal(grants[0].Chats, []string{"d2"}) {
		t.Fatalf("grants = %+v", grants)
	}
}
