package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeci/sieve/internal/store"
)

func fixture(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	sms, err := store.Open(ctx, filepath.Join(dir, "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sms.Close()
	for _, c := range []store.Container{
		{ID: "conv-family", Kind: "group", Name: "Family", IsGroup: true},
		{ID: "conv-private", Kind: "dm", Name: "Private"},
		{ID: "conv-bank", Kind: "dm", Name: "72975"},
	} {
		mustDo(t, sms.UpsertContainer(ctx, c))
	}
	for i, m := range []store.Message{
		{ID: "m1", ContainerID: "conv-family", SenderID: "+15550001", SenderName: "Mom", Body: "dinner at 7?"},
		{ID: "m2", ContainerID: "conv-family", SenderID: "+15550002", SenderName: "Hidden Cousin", Body: "secret gossip"},
		{ID: "m3", ContainerID: "conv-private", SenderID: "+15550003", Body: "private thing"},
		{ID: "m4", ContainerID: "conv-bank", SenderID: "72975", Body: "Your code is 482913"},
		{ID: "m5", ContainerID: "conv-family", SenderID: "+15550001", Body: "Your login code is 482913, don't share"},
	} {
		m.SentAt = at.Add(time.Duration(i) * time.Minute)
		mustDo(t, sms.UpsertMessage(ctx, m))
	}

	dc, err := store.Open(ctx, filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	for _, c := range []store.Container{
		{ID: "g-work", Kind: "guild", Name: "Work"},
		{ID: "c-general", Kind: "channel", ParentID: "g-work", Name: "general"},
		{ID: "g-fun", Kind: "guild", Name: "Fun"},
		{ID: "c-memes", Kind: "channel", ParentID: "g-fun", Name: "memes"},
	} {
		mustDo(t, dc.UpsertContainer(ctx, c))
	}
	mustDo(t, dc.UpsertMessage(ctx, store.Message{ID: "d1", ContainerID: "c-general", SenderID: "u1", Body: "standup", SentAt: at}))
	mustDo(t, dc.UpsertMessage(ctx, store.Message{ID: "d2", ContainerID: "c-memes", SenderID: "u2", Body: "lol", SentAt: at}))

	return map[string]string{
		"sms":      filepath.Join(dir, "sms.db"),
		"discord":  filepath.Join(dir, "discord.db"),
		"whatsapp": filepath.Join(dir, "missing-whatsapp.db"),
	}
}

const testExposure = `
[sms]
hide_containers = ["conv-private"]
hide_senders = ["+15550002"]

[discord]
hide_containers = ["g-work"]
`

func openTest(t *testing.T) *queryConn {
	t.Helper()
	pol, err := parseExposure(testExposure)
	if err != nil {
		t.Fatal(err)
	}
	q, err := openQueryConn(fixture(t), pol)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.close() })
	return q
}

func query(t *testing.T, q *queryConn, sql string, params ...any) (*queryResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return q.run(ctx, sql, params, 100)
}

func TestExposureFiltering(t *testing.T) {
	q := openTest(t)

	res, err := query(t, q, `SELECT id, body, is_auth FROM sms_messages ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]any{}
	for _, r := range res.Rows {
		got[r[0].(string)] = r[1:]
	}
	if _, ok := got["m2"]; ok {
		t.Error("message from hidden sender served")
	}
	if _, ok := got["m3"]; ok {
		t.Error("message in hidden container served")
	}
	if b := got["m1"]; b == nil || b[0] != "dinner at 7?" || b[1] != int64(0) {
		t.Errorf("ordinary message = %v, want body and is_auth=0", b)
	}
	for _, id := range []string{"m4", "m5"} {
		if b := got[id]; b == nil || b[0] != nil || b[1] != int64(1) {
			t.Errorf("auth message %s = %v, want NULL body and is_auth=1", id, b)
		}
	}

	res, err = query(t, q, `SELECT id FROM discord_messages`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "d2" {
		t.Errorf("discord messages = %v, want only d2 (hidden guild hides its channels)", res.Rows)
	}
	res, err = query(t, q, `SELECT id FROM discord_containers ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 || res.Rows[0][0] != "c-memes" || res.Rows[1][0] != "g-fun" {
		t.Errorf("discord containers = %v, want c-memes and g-fun", res.Rows)
	}
}

func TestBotCannotBypassViews(t *testing.T) {
	q := openTest(t)
	for _, sql := range []string{
		`SELECT * FROM src_sms.messages`,
		`SELECT body FROM src_sms.messages WHERE id = 'm2'`,
		`SELECT * FROM _hidden`,
		`SELECT sql FROM sqlite_temp_master`,
		`SELECT * FROM sqlite_schema`,
		`SELECT * FROM pragma_table_info('sms_messages')`,
		`SELECT * FROM pragma_database_list`,
		`WITH sms_messages AS (SELECT * FROM src_sms.messages) SELECT * FROM sms_messages`,
		`WITH x AS (SELECT * FROM src_sms.messages) SELECT * FROM x`,
		`SELECT * FROM sms_messages UNION ALL SELECT seq, id, container_id, NULL, NULL, sender_id, sender_name, sent_at, is_from_me, body, 0, media_type, reply_to_id, edited_at, deleted FROM src_sms.messages`,
		`SELECT (SELECT body FROM src_sms.messages LIMIT 1)`,
		`SELECT load_extension('x')`,
	} {
		if res, err := query(t, q, sql); err == nil {
			t.Errorf("%s: allowed, returned %v", sql, res.Rows)
		}
	}
}

func TestBotCannotWrite(t *testing.T) {
	q := openTest(t)
	for _, sql := range []string{
		`INSERT INTO _hidden VALUES ('sms', 'sender', 'x')`,
		`DELETE FROM _hidden`,
		`DROP VIEW sms_messages`,
		`CREATE TEMP VIEW leak AS SELECT * FROM src_sms.messages`,
		`CREATE TEMP TABLE t(x)`,
		`ATTACH DATABASE 'file:/tmp/x.db' AS x`,
		`DETACH DATABASE src_sms`,
		`PRAGMA query_only = 0`,
		`PRAGMA table_info(sms_messages)`,
		`SELECT 1; DELETE FROM _hidden`,
		`BEGIN`,
		`VACUUM`,
	} {
		if _, err := query(t, q, sql); err == nil {
			t.Errorf("%s: allowed", sql)
		}
	}
	// The policy survives the attempts.
	res, err := query(t, q, `SELECT count(*) FROM sms_messages WHERE sender_id = '+15550002'`)
	if err != nil || res.Rows[0][0] != int64(0) {
		t.Fatalf("hidden sender visible after write attempts: %v %v", res, err)
	}
}

func TestQueryFeatures(t *testing.T) {
	q := openTest(t)

	res, err := query(t, q, `
WITH recent AS (SELECT * FROM sms_messages WHERE sent_at >= ?1)
SELECT c.name, count(*) FROM recent r JOIN sms_containers c ON c.id = r.container_id GROUP BY c.name ORDER BY c.name`,
		"2026-10-01T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Errorf("grouped rows = %v, want Family and 72975", res.Rows)
	}

	ctx := context.Background()
	res, err = q.run(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 50) SELECT i FROM n`, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || !res.Truncated {
		t.Errorf("row cap: got %d rows truncated=%v, want 10 truncated", len(res.Rows), res.Truncated)
	}

	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = q.run(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n) SELECT count(*) FROM n`, nil, 10)
	if err == nil {
		t.Fatal("runaway query was not interrupted")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("interrupt took %v", time.Since(start))
	}

	if _, err := query(t, q, `SELECT * FROM whatsapp_messages`); err == nil {
		t.Error("view for missing source exists")
	}
}

func TestParseExposureStrict(t *testing.T) {
	if _, err := parseExposure("[sms]\nhide_sender = [\"+1\"]\n"); err == nil || !strings.Contains(err.Error(), "unknown keys") {
		t.Errorf("typo accepted: %v", err)
	}
	if _, err := parseExposure("[auth]\nextra_body_patterns = [\"(\"]\n"); err == nil {
		t.Error("invalid regex accepted")
	}
	if _, err := parseExposure(""); err != nil {
		t.Errorf("empty config rejected: %v", err)
	}
}

func TestHTTP(t *testing.T) {
	dir := t.TempDir()
	exposure := filepath.Join(dir, "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	token := strings.Repeat("t", 40)
	h := newHandler(Config{Sources: fixture(t), ExposureFile: exposure, MaxRows: 100, Timeout: 2 * time.Second}, token)

	do := func(method, path, auth, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("POST", "/v1/query", "", `{"sql":"SELECT 1"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := do("POST", "/v1/query", "Bearer wrong", `{"sql":"SELECT 1"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", rec.Code)
	}
	rec := do("POST", "/v1/query", "Bearer "+token, `{"sql":"SELECT count(*) FROM sms_messages"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("query: %d %s", rec.Code, rec.Body)
	}
	var res queryResult
	mustDo(t, json.Unmarshal(rec.Body.Bytes(), &res))
	if len(res.Rows) != 1 || res.Rows[0][0] != float64(3) {
		t.Errorf("count = %v, want 3", res.Rows)
	}

	rec = do("GET", "/v1/schema", "Bearer "+token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sms_messages"`) || strings.Contains(rec.Body.String(), "_hidden") {
		t.Errorf("schema: %d %s", rec.Code, rec.Body)
	}

	// A broken exposure file fails closed instead of serving unfiltered data.
	time.Sleep(10 * time.Millisecond)
	mustDo(t, os.WriteFile(exposure, []byte("[sms]\nhide_sendrs = []\n"), 0o600))
	if rec := do("POST", "/v1/query", "Bearer "+token, `{"sql":"SELECT 1"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("broken exposure: %d %s", rec.Code, rec.Body)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
