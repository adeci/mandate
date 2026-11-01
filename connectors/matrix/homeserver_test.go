package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/id"
)

const (
	owner    = id.UserID("@owner:example.org")
	bob      = id.UserID("@bob:example.org")
	carol    = id.UserID("@carol:example.org")
	password = "correct horse"
)

type device struct {
	user id.UserID
	id   id.DeviceID
}

type sentEvent struct {
	room    id.RoomID
	typ     string
	txn     string
	content json.RawMessage
}

// sessionData is a pointer so its keys marshal with their MarshalJSON.
type sessionData = *backup.EncryptedSessionData[backup.MegolmSessionData]

// homeserver is a fake Matrix homeserver: enough of the client-server API for
// logging in, end-to-end encryption, key backup, sync, history and sending.
type homeserver struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	changed chan struct{} // closed and replaced whenever a sync may have news

	tokens      map[string]device
	logins      []mautrix.ReqLogin
	loggedOut   []id.DeviceID
	accountData map[string]json.RawMessage
	deviceKeys  map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys
	otks        map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey
	master      map[id.UserID]mautrix.CrossSigningKeys
	selfSigning map[id.UserID]mautrix.CrossSigningKeys
	userSigning map[id.UserID]mautrix.CrossSigningKeys
	signatures  []mautrix.ReqUploadSignatures

	backupVersion *mautrix.RespRoomKeysVersion[backup.MegolmAuthData]
	backupKeys    mautrix.RespRoomKeys[sessionData]

	batches  []map[string]any // sync responses; since=N returns batches[N]
	since    int              // the newest since a sync asked with
	inbox    map[id.DeviceID][]map[string]any
	holdMail bool // keep to-device messages back

	history map[id.RoomID][]map[string]any // newest first, for /messages
	members map[id.RoomID][]map[string]any // lazy-loaded members for /messages
	media   map[string][]byte              // by mxc:// URI

	sends    []sentEvent
	sendMode string // "", "forbidden", "hangup"
}

func newHomeserver(t *testing.T) *homeserver {
	h := &homeserver{
		t:           t,
		changed:     make(chan struct{}),
		tokens:      map[string]device{},
		accountData: map[string]json.RawMessage{},
		deviceKeys:  map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys{},
		master:      map[id.UserID]mautrix.CrossSigningKeys{},
		selfSigning: map[id.UserID]mautrix.CrossSigningKeys{},
		userSigning: map[id.UserID]mautrix.CrossSigningKeys{},
		otks:        map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey{},
		inbox:       map[id.DeviceID][]map[string]any{},
		history:     map[id.RoomID][]map[string]any{},
		members:     map[id.RoomID][]map[string]any{},
		media:       map[string][]byte{},
	}
	mux := http.NewServeMux()
	open := map[string]http.HandlerFunc{
		"GET /_matrix/client/v3/login":  h.loginFlows,
		"POST /_matrix/client/v3/login": h.login,
	}
	authed := map[string]func(http.ResponseWriter, *http.Request, device){
		"POST /_matrix/client/v3/logout":                         h.logout,
		"GET /_matrix/client/v3/user/{user}/account_data/{type}": h.getAccountData,
		"PUT /_matrix/client/v3/user/{user}/account_data/{type}": h.putAccountData,
		"POST /_matrix/client/v3/keys/upload":                    h.keysUpload,
		"POST /_matrix/client/v3/keys/query":                     h.keysQuery,
		"POST /_matrix/client/v3/keys/claim":                     h.keysClaim,
		"POST /_matrix/client/v3/keys/device_signing/upload":     h.deviceSigningUpload,
		"POST /_matrix/client/v3/keys/signatures/upload":         h.signaturesUpload,
		"GET /_matrix/client/v3/room_keys/version":               h.backupVersionGet,
		"GET /_matrix/client/v3/room_keys/keys":                  h.backupKeysGet,
		"PUT /_matrix/client/v3/sendToDevice/{type}/{txn}":       h.sendToDevice,
		"GET /_matrix/client/v3/sync":                            h.sync,
		"GET /_matrix/client/v3/rooms/{room}/messages":           h.messages,
		"GET /_matrix/client/v3/rooms/{room}/context/{event}":    h.context,
		"GET /_matrix/client/v3/rooms/{room}/event/{event}":      h.event,
		"GET /_matrix/client/v1/media/download/{server}/{file}":  h.download,
		"PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}":  h.send,
	}
	for pattern, f := range open {
		mux.HandleFunc(pattern, f)
	}
	for pattern, f := range authed {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			dev, ok := h.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			h.mu.Unlock()
			if !ok {
				matrixError(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN")
				return
			}
			f(w, r, dev)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("homeserver: unhandled %s %s", r.Method, r.URL.Path)
		matrixError(w, http.StatusNotFound, "M_UNRECOGNIZED")
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func matrixError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"errcode": code, "error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		panic(err)
	}
}

// notify wakes waiting syncs; h.mu must be held.
func (h *homeserver) notify() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// addDevice registers a device of user, returning its access token.
func (h *homeserver) addDevice(user id.UserID, dev id.DeviceID) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	token := "token-" + string(dev)
	h.tokens[token] = device{user, dev}
	return token
}

func (h *homeserver) loginFlows(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, mautrix.RespLoginFlows{Flows: []mautrix.LoginFlow{{Type: mautrix.AuthTypePassword}}})
}

func (h *homeserver) login(w http.ResponseWriter, r *http.Request) {
	var req mautrix.ReqLogin
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logins = append(h.logins, req)
	if id.UserID(req.Identifier.User) != owner || req.Password != password {
		matrixError(w, http.StatusForbidden, "M_FORBIDDEN")
		return
	}
	dev := id.DeviceID(fmt.Sprintf("MANDATE%d", len(h.logins)))
	token := "token-" + string(dev)
	h.tokens[token] = device{owner, dev}
	writeJSON(w, http.StatusOK, mautrix.RespLogin{AccessToken: token, DeviceID: dev, UserID: owner})
}

func (h *homeserver) logout(w http.ResponseWriter, r *http.Request, dev device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	delete(h.deviceKeys[dev.user], dev.id)
	h.loggedOut = append(h.loggedOut, dev.id)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (h *homeserver) getAccountData(w http.ResponseWriter, r *http.Request, _ device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.accountData[r.PathValue("type")]
	if !ok {
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (h *homeserver) putAccountData(w http.ResponseWriter, r *http.Request, _ device) {
	b, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accountData[r.PathValue("type")] = b
	writeJSON(w, http.StatusOK, struct{}{})
}

func (h *homeserver) keysUpload(w http.ResponseWriter, r *http.Request, dev device) {
	var req mautrix.ReqUploadKeys
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.DeviceKeys != nil {
		if h.deviceKeys[dev.user] == nil {
			h.deviceKeys[dev.user] = map[id.DeviceID]mautrix.DeviceKeys{}
		}
		h.deviceKeys[dev.user][dev.id] = *req.DeviceKeys
	}
	if h.otks[dev.id] == nil {
		h.otks[dev.id] = map[id.KeyID]mautrix.OneTimeKey{}
	}
	maps.Copy(h.otks[dev.id], req.OneTimeKeys)
	writeJSON(w, http.StatusOK, mautrix.RespUploadKeys{OneTimeKeyCounts: mautrix.OTKCount{SignedCurve25519: len(h.otks[dev.id])}})
}

func (h *homeserver) keysQuery(w http.ResponseWriter, r *http.Request, _ device) {
	var req mautrix.ReqQueryKeys
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := mautrix.RespQueryKeys{
		DeviceKeys:      map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys{},
		MasterKeys:      map[id.UserID]mautrix.CrossSigningKeys{},
		SelfSigningKeys: map[id.UserID]mautrix.CrossSigningKeys{},
		UserSigningKeys: map[id.UserID]mautrix.CrossSigningKeys{},
	}
	for user := range req.DeviceKeys {
		resp.DeviceKeys[user] = map[id.DeviceID]mautrix.DeviceKeys{}
		maps.Copy(resp.DeviceKeys[user], h.deviceKeys[user])
		if m, ok := h.master[user]; ok {
			resp.MasterKeys[user] = m
			resp.SelfSigningKeys[user] = h.selfSigning[user]
			resp.UserSigningKeys[user] = h.userSigning[user]
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *homeserver) keysClaim(w http.ResponseWriter, r *http.Request, _ device) {
	var req mautrix.ReqClaimKeys
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := mautrix.RespClaimKeys{OneTimeKeys: map[id.UserID]map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey{}}
	for user, devices := range req.OneTimeKeys {
		resp.OneTimeKeys[user] = map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey{}
		for dev := range devices {
			for keyID, key := range h.otks[dev] {
				delete(h.otks[dev], keyID)
				resp.OneTimeKeys[user][dev] = map[id.KeyID]mautrix.OneTimeKey{keyID: key}
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *homeserver) deviceSigningUpload(w http.ResponseWriter, r *http.Request, dev device) {
	var req mautrix.UploadCrossSigningKeysReq[any]
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.master[dev.user], h.selfSigning[dev.user], h.userSigning[dev.user] = req.Master, req.SelfSigning, req.UserSigning
	writeJSON(w, http.StatusOK, struct{}{})
}

// signaturesUpload adds signatures of devices to their keys.
func (h *homeserver) signaturesUpload(w http.ResponseWriter, r *http.Request, _ device) {
	var req mautrix.ReqUploadSignatures
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signatures = append(h.signatures, req)
	for user, keys := range req {
		for keyID, s := range keys {
			dk, ok := h.deviceKeys[user][id.DeviceID(keyID)]
			if !ok {
				continue
			}
			for signer, sigs := range s.Signatures {
				if dk.Signatures[signer] == nil {
					dk.Signatures[signer] = map[id.KeyID]string{}
				}
				maps.Copy(dk.Signatures[signer], sigs)
			}
		}
	}
	writeJSON(w, http.StatusOK, mautrix.RespUploadSignatures{})
}

func (h *homeserver) backupVersionGet(w http.ResponseWriter, _ *http.Request, _ device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.backupVersion == nil {
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
		return
	}
	writeJSON(w, http.StatusOK, h.backupVersion)
}

func (h *homeserver) backupKeysGet(w http.ResponseWriter, _ *http.Request, _ device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	writeJSON(w, http.StatusOK, h.backupKeys)
}

func (h *homeserver) sendToDevice(w http.ResponseWriter, r *http.Request, from device) {
	var req struct {
		Messages map[id.UserID]map[id.DeviceID]json.RawMessage `json:"messages"`
	}
	decode(r, &req)
	h.mu.Lock()
	defer h.mu.Unlock()
	for user, devices := range req.Messages {
		for dev, content := range devices {
			targets := []id.DeviceID{dev}
			if dev == "*" {
				targets = slices.Collect(maps.Keys(h.deviceKeys[user]))
			}
			for _, t := range targets {
				h.inbox[t] = append(h.inbox[t], map[string]any{
					"type": r.PathValue("type"), "sender": from.user, "content": content,
				})
			}
		}
	}
	h.notify()
	writeJSON(w, http.StatusOK, struct{}{})
}

// push queues a sync response and returns the since token that follows it.
func (h *homeserver) push(batch map[string]any) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batches = append(h.batches, batch)
	h.notify()
	return len(h.batches)
}

func (h *homeserver) sync(w http.ResponseWriter, r *http.Request, dev device) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	timeout, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	deadline := time.After(time.Duration(timeout) * time.Millisecond)
	for {
		h.mu.Lock()
		h.since = max(h.since, since)
		var resp map[string]any
		switch {
		case since < len(h.batches):
			resp = maps.Clone(h.batches[since])
			since++
		case len(h.inbox[dev.id]) > 0 && !h.holdMail:
			resp = map[string]any{}
		}
		if resp != nil {
			if !h.holdMail && len(h.inbox[dev.id]) > 0 {
				resp["to_device"] = map[string]any{"events": h.inbox[dev.id]}
				h.inbox[dev.id] = nil
			}
			resp["next_batch"] = strconv.Itoa(since)
		}
		changed := h.changed
		h.mu.Unlock()
		if resp != nil {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		select {
		case <-changed:
		case <-deadline:
			writeJSON(w, http.StatusOK, map[string]any{"next_batch": strconv.Itoa(since)})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// waitSynced waits until the connector has stored the first n batches.
func (h *homeserver) waitSynced(n int) {
	h.t.Helper()
	for range 500 {
		h.mu.Lock()
		done := h.since >= n
		h.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("connector did not sync past batch %d", n)
}

// messages pages through h.history, whose tokens are offsets.
func (h *homeserver) messages(w http.ResponseWriter, r *http.Request, _ device) {
	room := id.RoomID(r.PathValue("room"))
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	h.mu.Lock()
	defer h.mu.Unlock()
	all := h.history[room]
	from = min(from, len(all))
	end := min(from+limit, len(all))
	resp := map[string]any{"start": strconv.Itoa(from), "chunk": all[from:end], "state": h.members[room]}
	if end < len(all) {
		resp["end"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, resp)
}

// locked reads h's state under its lock.
func locked[T any](h *homeserver, read func() T) T {
	h.mu.Lock()
	defer h.mu.Unlock()
	return read()
}

func (h *homeserver) context(w http.ResponseWriter, r *http.Request, _ device) {
	room := id.RoomID(r.PathValue("room"))
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, evt := range h.history[room] {
		if evt["event_id"] == r.PathValue("event") {
			writeJSON(w, http.StatusOK, map[string]any{"event": evt, "start": strconv.Itoa(i + 1), "end": strconv.Itoa(i)})
			return
		}
	}
	matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
}

func (h *homeserver) event(w http.ResponseWriter, r *http.Request, _ device) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, evt := range h.history[id.RoomID(r.PathValue("room"))] {
		if evt["event_id"] == r.PathValue("event") {
			writeJSON(w, http.StatusOK, evt)
			return
		}
	}
	matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
}

func (h *homeserver) download(w http.ResponseWriter, r *http.Request, _ device) {
	h.mu.Lock()
	data, ok := h.media["mxc://"+r.PathValue("server")+"/"+r.PathValue("file")]
	h.mu.Unlock()
	if !ok {
		matrixError(w, http.StatusNotFound, "M_NOT_FOUND")
		return
	}
	w.Write(data)
}

func (h *homeserver) send(w http.ResponseWriter, r *http.Request, _ device) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.sends = append(h.sends, sentEvent{room: id.RoomID(r.PathValue("room")), typ: r.PathValue("type"), txn: r.PathValue("txn"), content: body})
	n, mode := len(h.sends), h.sendMode
	h.mu.Unlock()
	switch mode {
	case "forbidden":
		matrixError(w, http.StatusForbidden, "M_FORBIDDEN")
	case "hangup":
		// The request arrived; the response never does.
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	default:
		writeJSON(w, http.StatusOK, map[string]string{"event_id": fmt.Sprintf("$sent%d", n)})
	}
}

func (h *homeserver) sent() []sentEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentEvent(nil), h.sends...)
}

// Event builders for sync batches and history.

func msgEvent(eventID string, sender id.UserID, ts int64, content map[string]any) map[string]any {
	return map[string]any{"type": "m.room.message", "event_id": eventID, "sender": sender, "origin_server_ts": ts, "content": content}
}

func textEvent(eventID string, sender id.UserID, ts int64, body string) map[string]any {
	return msgEvent(eventID, sender, ts, map[string]any{"msgtype": "m.text", "body": body})
}

func stateEvent(typ, stateKey string, sender id.UserID, content map[string]any) map[string]any {
	return map[string]any{
		"type": typ, "state_key": stateKey, "sender": sender, "content": content,
		"event_id": "$state-" + typ + "-" + stateKey, "origin_server_ts": 1,
	}
}

func member(user id.UserID, name string) map[string]any {
	return stateEvent("m.room.member", user.String(), user, map[string]any{"membership": "join", "displayname": name})
}

func joined(rooms map[id.RoomID]map[string]any) map[string]any {
	return map[string]any{"rooms": map[string]any{"join": rooms}}
}

func room(state, timeline []map[string]any) map[string]any {
	return map[string]any{"state": map[string]any{"events": state}, "timeline": map[string]any{"events": timeline}}
}
