package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	goodToken = "fmu1-good"
	accountID = "u1"
)

type change struct {
	state int
	id    string
	kind  string // created, updated, destroyed
}

type call struct {
	name string
	args map[string]any
}

// fakeJMAP is a Fastmail account: its session, mailboxes, identities and
// emails, with a change log for Email/changes.
type fakeJMAP struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	token        string
	noSubmission bool
	readOnly     bool
	mailboxes    []mailbox
	identities   []identity
	emails       map[string]*email
	blobs        map[string]string
	log          []change
	state        int
	minState     int    // Email/changes refuses older states
	queryCap     int    // most ids one Email/query returns, if set
	submit       string // "", "refuse", "error" or "hangup"
	calls        []call // Email/set and EmailSubmission/set
	nextID       int
}

func newFakeJMAP(t *testing.T) *fakeJMAP {
	f := &fakeJMAP{
		t: t, token: goodToken, emails: map[string]*email{}, blobs: map[string]string{},
		mailboxes: []mailbox{
			{ID: "mb-inbox", Role: "inbox"},
			{ID: "mb-sent", Role: "sent"},
			{ID: "mb-drafts", Role: "drafts"},
			{ID: "mb-trash", Role: "trash"},
			{ID: "mb-junk", Role: "junk"},
			{ID: "mb-archive"},
		},
		identities: []identity{
			{ID: "id-wild", Name: "Owner", Email: "*@owner.dev"},
			{ID: "id-main", Name: "Owner Name", Email: "owner@fastmail.com"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/session", f.session)
	mux.HandleFunc("POST /jmap/api", f.api)
	mux.HandleFunc("GET /jmap/download/{account}/{blob}/{name}", f.download)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJMAP) authorized(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	ok := r.Header.Get("Authorization") == "Bearer "+f.token
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return ok
}

func (f *fakeJMAP) session(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	primary := map[string]string{capMail: accountID}
	if !f.noSubmission {
		primary[capSubmission] = accountID
	}
	writeJSON(w, map[string]any{
		"apiUrl":          f.srv.URL + "/jmap/api",
		"downloadUrl":     f.srv.URL + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"username":        "owner@fastmail.com",
		"primaryAccounts": primary,
		"accounts":        map[string]any{accountID: map[string]any{"isReadOnly": f.readOnly}},
	})
}

func (f *fakeJMAP) download(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	f.mu.Lock()
	blob, ok := f.blobs[r.PathValue("blob")]
	f.mu.Unlock()
	if r.PathValue("account") != accountID || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", r.URL.Query().Get("type"))
	w.Write([]byte(blob))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// add stores e and logs it as created.
func (f *fakeJMAP) add(e email) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails[e.ID] = &e
	f.record(e.ID, "created")
}

// update changes the email id and logs it.
func (f *fakeJMAP) update(id string, fn func(*email)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.emails[id])
	f.record(id, "updated")
}

func (f *fakeJMAP) destroy(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.emails, id)
	f.record(id, "destroyed")
}

func (f *fakeJMAP) record(id, kind string) {
	f.state++
	f.log = append(f.log, change{f.state, id, kind})
}

func (f *fakeJMAP) currentState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strconv.Itoa(f.state)
}

func (f *fakeJMAP) set(fn func(*fakeJMAP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeJMAP) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeJMAP) api(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(w, r) {
		return
	}
	var req struct {
		Using       []string             `json:"using"`
		MethodCalls [][3]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	created := map[string]string{} // creation ID → email ID
	var out []any
	for _, c := range req.MethodCalls {
		var name, id string
		var args map[string]any
		json.Unmarshal(c[0], &name)
		json.Unmarshal(c[1], &args)
		json.Unmarshal(c[2], &id)
		if args["accountId"] != accountID {
			out = append(out, []any{"error", map[string]any{"type": "accountNotFound"}, id})
			continue
		}
		switch name {
		case "Mailbox/get":
			out = append(out, []any{name, map[string]any{"state": "m", "list": f.mailboxes}, id})
		case "Identity/get":
			out = append(out, []any{name, map[string]any{"state": "i", "list": f.identities}, id})
		case "Email/get":
			out = append(out, []any{name, f.get(args), id})
		case "Email/query":
			out = append(out, []any{name, f.query(args), id})
		case "Email/changes":
			since, _ := strconv.Atoi(args["sinceState"].(string))
			if since < f.minState {
				out = append(out, []any{"error", map[string]any{"type": "cannotCalculateChanges"}, id})
				continue
			}
			out = append(out, []any{name, f.changes(since), id})
		case "Email/set":
			f.calls = append(f.calls, call{name, args})
			out = append(out, []any{name, f.setEmails(args, created), id})
		case "EmailSubmission/set":
			f.calls = append(f.calls, call{name, args})
			switch f.submit {
			case "hangup":
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			case "error":
				out = append(out, []any{"error", map[string]any{"type": "forbidden"}, id})
			case "refuse":
				out = append(out, []any{name, map[string]any{"notCreated": map[string]any{
					"send": map[string]any{"type": "forbiddenToSend", "description": "sending limit reached"},
				}}, id})
			default:
				out = append(out, f.submitEmails(args, created, id)...)
			}
		default:
			out = append(out, []any{"error", map[string]any{"type": "unknownMethod"}, id})
		}
	}
	writeJSON(w, map[string]any{"methodResponses": out, "sessionState": "s"})
}

func (f *fakeJMAP) get(args map[string]any) map[string]any {
	ids, _ := args["ids"].([]any)
	limit, _ := args["maxBodyValueBytes"].(float64)
	list := []email{}
	var notFound []string
	for _, v := range ids {
		e, ok := f.emails[v.(string)]
		if !ok {
			notFound = append(notFound, v.(string))
			continue
		}
		c := *e
		c.BodyValues = map[string]bodyValue{}
		if args["fetchTextBodyValues"] == true {
			for k, bv := range e.BodyValues {
				if limit > 0 && len(bv.Value) > int(limit) {
					bv = bodyValue{Value: bv.Value[:int(limit)], IsTruncated: true}
				}
				c.BodyValues[k] = bv
			}
		}
		list = append(list, c)
	}
	return map[string]any{"state": strconv.Itoa(f.state), "list": list, "notFound": notFound}
}

func (f *fakeJMAP) query(args map[string]any) map[string]any {
	filter := args["filter"].(map[string]any)
	after, err := time.Parse(time.RFC3339, filter["after"].(string))
	if err != nil {
		f.t.Errorf("query after: %v", err)
	}
	other, _ := filter["inMailboxOtherThan"].([]any)
	var matches []*email
	for _, e := range f.emails {
		if e.ReceivedAt.Before(after) {
			continue
		}
		for mb := range e.MailboxIDs {
			if !slices.Contains(other, any(mb)) {
				matches = append(matches, e)
				break
			}
		}
	}
	slices.SortFunc(matches, func(a, b *email) int { return b.ReceivedAt.Compare(a.ReceivedAt) })
	pos, limit := int(args["position"].(float64)), int(args["limit"].(float64))
	if f.queryCap > 0 {
		limit = min(limit, f.queryCap)
	}
	ids := []string{}
	for _, e := range matches[min(pos, len(matches)):min(pos+limit, len(matches))] {
		ids = append(ids, e.ID)
	}
	return map[string]any{"ids": ids, "position": pos}
}

func (f *fakeJMAP) changes(since int) map[string]any {
	res := map[string][]string{"created": {}, "updated": {}, "destroyed": {}}
	for _, c := range f.log {
		if c.state > since && !slices.Contains(res[c.kind], c.id) {
			res[c.kind] = append(res[c.kind], c.id)
		}
	}
	return map[string]any{
		"oldState": strconv.Itoa(since), "newState": strconv.Itoa(f.state), "hasMoreChanges": false,
		"created": res["created"], "updated": res["updated"], "destroyed": res["destroyed"],
	}
}

func (f *fakeJMAP) setEmails(args map[string]any, created map[string]string) map[string]any {
	res := map[string]any{}
	if create, ok := args["create"].(map[string]any); ok {
		done := map[string]any{}
		for cid, v := range create {
			b, _ := json.Marshal(v)
			var e email
			json.Unmarshal(b, &e)
			f.nextID++
			e.ID = fmt.Sprintf("new-%d", f.nextID)
			e.ThreadID = "t-" + e.ID
			for _, other := range f.emails {
				if len(e.InReplyTo) > 0 && slices.Contains(other.MessageID, e.InReplyTo[0]) {
					e.ThreadID = other.ThreadID
				}
			}
			e.MessageID = []string{e.ID + "@fastmail.example"}
			e.ReceivedAt = time.Now().UTC()
			f.emails[e.ID] = &e
			f.record(e.ID, "created")
			created[cid] = e.ID
			done[cid] = map[string]any{"id": e.ID}
		}
		res["created"] = done
	}
	if destroy, ok := args["destroy"].([]any); ok {
		for _, id := range destroy {
			delete(f.emails, id.(string))
			f.record(id.(string), "destroyed")
		}
		res["destroyed"] = destroy
	}
	return res
}

// submitEmails sends, then applies onSuccessUpdateEmail as Fastmail does.
func (f *fakeJMAP) submitEmails(args map[string]any, created map[string]string, id string) []any {
	done := map[string]any{}
	updated := map[string]any{}
	for cid, v := range args["create"].(map[string]any) {
		emailID := v.(map[string]any)["emailId"].(string)
		if strings.HasPrefix(emailID, "#") {
			emailID = created[emailID[1:]]
		}
		e := f.emails[emailID]
		patch, _ := args["onSuccessUpdateEmail"].(map[string]any)["#"+cid].(map[string]any)
		for path, val := range patch {
			field, key, _ := strings.Cut(path, "/")
			m := e.MailboxIDs
			if field == "keywords" {
				m = e.Keywords
			}
			if val == nil {
				delete(m, key)
			} else {
				m[key] = true
			}
		}
		f.record(emailID, "updated")
		f.nextID++
		done[cid] = map[string]any{"id": fmt.Sprintf("sub-%d", f.nextID)}
		updated[emailID] = nil
	}
	return []any{
		[]any{"EmailSubmission/set", map[string]any{"created": done}, id},
		[]any{"Email/set", map[string]any{"updated": updated}, id},
	}
}
