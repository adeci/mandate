package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
)

func fixture(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	wa, err := store.Open(ctx, filepath.Join(dir, "whatsapp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer wa.Close()
	for _, c := range []store.Container{
		{ID: "conv-family", Kind: "group", Name: "Family", IsGroup: true},
		{ID: "conv-private", Kind: "dm", Name: "Private"},
		{ID: "conv-bank", Kind: "dm", Name: "72975"},
		{ID: "+15550002", Kind: "dm", Name: "Hidden Cousin"},
	} {
		mustDo(t, wa.UpsertContainer(ctx, c))
	}
	for i, m := range []store.Message{
		{ID: "m1", ContainerID: "conv-family", SenderID: "+15550001", SenderName: "Mom", Body: "dinner at 7?", ReplyToID: "m5"},
		{ID: "m2", ContainerID: "conv-family", SenderID: "+15550002", SenderName: "Hidden Cousin", Body: "secret gossip"},
		{ID: "m3", ContainerID: "conv-private", SenderID: "+15550003", Body: "private thing"},
		{ID: "m4", ContainerID: "conv-bank", SenderID: "72975", Body: "Your code is 482913"},
		{ID: "m5", ContainerID: "conv-family", SenderID: "+15550001", Body: "Your login code is 482913, don't share", ReplyToID: "m2"},
		{ID: "m6", ContainerID: "+15550002", SenderID: "+15559990000", IsFromMe: true, Body: "dinner with you?"},
	} {
		m.SentAt = at.Add(time.Duration(i) * time.Minute)
		mustDo(t, wa.UpsertMessage(ctx, m))
	}

	dc, err := store.Open(ctx, filepath.Join(dir, "discord.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	// Threads hang under channels under guilds; a DM's ID is not its other
	// participant's, which the participants or the messages say.
	for _, c := range []store.Container{
		{ID: "g-work", Kind: "guild", Name: "Work"},
		{ID: "c-general", Kind: "channel", ParentID: "g-work", Name: "general"},
		{ID: "t-standup", Kind: "thread", ParentID: "c-general", Name: "standup"},
		{ID: "g-fun", Kind: "guild", Name: "Fun"},
		{ID: "c-memes", Kind: "channel", ParentID: "g-fun", Name: "memes"},
		{ID: "t-cats", Kind: "thread", ParentID: "c-memes", Name: "cats"},
		{ID: "c-secret", Kind: "channel", ParentID: "g-fun", Name: "secret"},
		{ID: "t-plans", Kind: "thread", ParentID: "c-secret", Name: "plans"},
		{ID: "dm-known", Kind: "dm", Name: "Hidden", Participants: []store.Participant{{ID: "u-hidden"}}},
		{ID: "dm-unknown", Kind: "dm", Name: "Hidden too"},
	} {
		mustDo(t, dc.UpsertContainer(ctx, c))
	}
	for _, m := range []store.Message{
		{ID: "d1", ContainerID: "c-general", SenderID: "u1", Body: "standup"},
		{ID: "d2", ContainerID: "c-memes", SenderID: "u2", Body: "lol"},
		{ID: "d6", ContainerID: "t-standup", SenderID: "u1", Body: "thread notes"},
		{ID: "d7", ContainerID: "t-plans", SenderID: "u2", Body: "secret plans"},
		{ID: "d8", ContainerID: "dm-known", SenderID: "u-me", IsFromMe: true, Body: "hey, about the plans"},
		{ID: "d9", ContainerID: "dm-unknown", SenderID: "u-hidden", Body: "hi"},
		{ID: "d10", ContainerID: "dm-unknown", SenderID: "u-me", IsFromMe: true, Body: "hey you"},
	} {
		m.SentAt = at
		mustDo(t, dc.UpsertMessage(ctx, m))
	}

	return map[string]string{
		"whatsapp": filepath.Join(dir, "whatsapp.db"),
		"discord":  filepath.Join(dir, "discord.db"),
	}
}

const testExposure = `
[whatsapp]
hide_containers = ["conv-private"]
hide_senders = ["+15550002"]

[discord]
hide_containers = ["g-work", "c-secret"]
hide_senders = ["u-hidden"]
`

func sourceHandler(t *testing.T) (*handler, string) {
	t.Helper()
	exposure := filepath.Join(t.TempDir(), "exposure.toml")
	mustDo(t, os.WriteFile(exposure, []byte(testExposure), 0o600))
	return newHandler(Config{Sources: fixture(t), ExposureFile: exposure, Timeout: 2 * time.Second}, testToken), exposure
}

func getTable(t *testing.T, h *handler, path string) (int, table, string) {
	t.Helper()
	rec := call(h, http.MethodGet, path, "")
	var res table
	if rec.Code == http.StatusOK {
		mustDo(t, json.Unmarshal(rec.Body.Bytes(), &res))
	}
	return rec.Code, res, rec.Body.String()
}

func column(t *testing.T, res table, name string) []any {
	t.Helper()
	i := slices.Index(res.Columns, name)
	if i < 0 {
		t.Fatalf("no column %s in %v", name, res.Columns)
	}
	var out []any
	for _, r := range res.Rows {
		out = append(out, r[i])
	}
	return out
}

func TestVerbsNeverShowHiddenChatsOrLoginCodes(t *testing.T) {
	t.Parallel()
	h, _ := sourceHandler(t)

	_, res, _ := getTable(t, h, "/v1/whatsapp/chats")
	if got := column(t, res, "id"); !slices.Equal(got, []any{"conv-bank", "conv-family"}) && !slices.Equal(got, []any{"conv-family", "conv-bank"}) {
		t.Errorf("whatsapp chats = %v, want family and bank (hidden chat and hidden sender's DM dropped)", got)
	}
	_, res, _ = getTable(t, h, "/v1/discord/chats")
	got := column(t, res, "name")
	slices.SortFunc(got, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	if want := []any{"Fun", "Fun / memes", "Fun / memes / cats"}; !slices.Equal(got, want) {
		t.Errorf("discord chats = %v, want %v: threads named under their channel and guild, hidden ones and what is under them dropped", got, want)
	}
	_, res, _ = getTable(t, h, "/v1/discord/search?text=lol")
	if got := column(t, res, "chat"); !slices.Equal(got, []any{"Fun / memes"}) {
		t.Errorf("discord search chat = %v", got)
	}

	_, res, _ = getTable(t, h, "/v1/whatsapp/messages?chat=conv-family")
	if got := column(t, res, "body"); !slices.Equal(got, []any{"[login code hidden]", "dinner at 7?"}) {
		t.Errorf("family messages = %v, want the login code replaced and the hidden sender dropped", got)
	}
	if got := column(t, res, "reply_to"); !slices.Equal(got, []any{nil, float64(5)}) {
		t.Errorf("family reply_to = %v, want nothing for the reply to a hidden sender", got)
	}

	for _, chat := range []string{"conv-private", "+15550002", "c-general", "nope"} {
		source := "whatsapp"
		if strings.HasPrefix(chat, "c-") {
			source = "discord"
		}
		code, _, body := getTable(t, h, "/v1/"+source+"/messages?chat="+url.QueryEscape(chat))
		if code != http.StatusNotFound || !strings.Contains(body, "unknown chat") || !strings.Contains(body, "chats --search") {
			t.Errorf("messages in %s: %d %s, want 404 unknown chat with a hint", chat, code, body)
		}
		if code, _, _ := getTable(t, h, "/v1/"+source+"/search?text=a&chat="+url.QueryEscape(chat)); code != http.StatusNotFound {
			t.Errorf("search in %s: %d, want 404", chat, code)
		}
	}

	for text, want := range map[string][]any{
		"dinner":  {"dinner at 7?"},
		"482913":  nil,
		"code":    nil,
		"gossip":  nil,
		"private": nil,
	} {
		_, res, _ := getTable(t, h, "/v1/whatsapp/search?text="+text)
		if got := column(t, res, "body"); !slices.Equal(got, want) {
			t.Errorf("search %q = %v, want %v", text, got, want)
		}
	}
}

func TestVerbsPaginate(t *testing.T) {
	t.Parallel()
	h, _ := sourceHandler(t)
	_, res, _ := getTable(t, h, "/v1/whatsapp/messages?chat=conv-family&limit=1")
	if !res.More || res.NextBefore != 5 || len(res.Rows) != 1 {
		t.Fatalf("first page: %+v, want one row and next_before 5", res)
	}
	_, res, _ = getTable(t, h, "/v1/whatsapp/messages?chat=conv-family&limit=1&before=5")
	if res.More || len(res.Rows) != 1 || res.Rows[0][0] != float64(1) {
		t.Errorf("second page: %+v, want seq 1 and no more", res)
	}
	_, res, _ = getTable(t, h, "/v1/whatsapp/chats?limit=1")
	if !res.More || res.NextBefore != 0 || len(res.Rows) != 1 {
		t.Errorf("chats page: %+v", res)
	}
	_, res, _ = getTable(t, h, "/v1/whatsapp/search?text=dinner&since=2026-10-02")
	if len(res.Rows) != 0 {
		t.Errorf("search since: %+v", res)
	}
	for _, q := range []string{
		"chats?limit=0", "chats?limit=501", "messages?chat=conv-family&before=x", "messages", "search", "search?text=a&since=yesterday",
	} {
		if code, _, body := getTable(t, h, "/v1/whatsapp/"+q); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", q, code, body)
		}
	}
}

// History sync stores older messages after newer ones.
func TestVerbsOrderBySentTimeAcrossIngestOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	st, err := store.Open(context.Background(), path)
	mustDo(t, err)
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c1", Kind: "dm"}))
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, m := range []struct {
		id string
		at time.Duration
	}{{"evening", 18*time.Hour + 40*time.Minute}, {"morning", 9*time.Hour + 12*time.Minute}, {"noon-a", 12 * time.Hour}, {"noon-b", 12 * time.Hour}} {
		mustDo(t, st.UpsertMessage(context.Background(), store.Message{ID: m.id, ContainerID: "c1", SenderID: "u1", Body: "x " + m.id, SentAt: day.Add(m.at)}))
	}
	// Stored after c1 but last active before it, though the service reported
	// older activity; c3 never had a message.
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c2", Kind: "dm", LastMessageAt: day.Add(5 * time.Hour)}))
	mustDo(t, st.UpsertMessage(context.Background(), store.Message{ID: "late", ContainerID: "c2", SenderID: "u2", Body: "late", SentAt: day.Add(10 * time.Hour)}))
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c3", Kind: "dm"}))
	// The service reports activity newer than anything stored.
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c4", Kind: "dm", LastMessageAt: day.Add(20 * time.Hour)}))
	mustDo(t, st.Close())
	h := newHandler(Config{Sources: map[string]string{"wa": path}, ExposureFile: filepath.Join(dir, "exposure.toml"), Timeout: 2 * time.Second}, testToken)

	for _, verb := range []string{"messages?chat=c1", "search?text=x"} {
		var got []any
		next := "&limit=2"
		for range 2 {
			_, res, _ := getTable(t, h, "/v1/wa/"+verb+next)
			got = append(got, column(t, res, "body")...)
			if !res.More {
				break
			}
			next = "&limit=2&before=" + strconv.FormatInt(res.NextBefore, 10)
		}
		want := []any{"x evening", "x noon-b", "x noon-a", "x morning"}
		if !slices.Equal(got, want) {
			t.Errorf("%s pages = %v, want %v", verb, got, want)
		}
	}

	_, res, _ := getTable(t, h, "/v1/wa/chats")
	if got, want := column(t, res, "id"), []any{"c4", "c1", "c2", "c3"}; !slices.Equal(got, want) {
		t.Errorf("chats = %v, want %v", got, want)
	}
	if got, want := column(t, res, "last_message"), []any{"2026-10-30 20:00", "2026-10-30 18:40", "2026-10-30 10:00", nil}; !slices.Equal(got, want) {
		t.Errorf("last_message = %v, want %v", got, want)
	}
}

// A connector built before last_message_at and media existed still lists its
// chats, dated by stored messages alone, and their messages.
func TestReadsOnDatabaseOfOlderConnector(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	st, err := store.Open(context.Background(), path)
	mustDo(t, err)
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c1", Kind: "dm"}))
	mustDo(t, st.UpsertMessage(context.Background(), store.Message{
		ID: "m", ContainerID: "c1", SenderID: "u1", Body: "hi",
		SentAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
	}))
	for _, q := range []string{`ALTER TABLE containers DROP COLUMN last_message_at`, `DROP TABLE media`} {
		_, err = st.DB().ExecContext(context.Background(), q)
		mustDo(t, err)
	}
	mustDo(t, st.Close())
	h := newHandler(Config{Sources: map[string]string{"wa": path}, ExposureFile: filepath.Join(dir, "exposure.toml"), Timeout: 2 * time.Second}, testToken)

	status, res, body := getTable(t, h, "/v1/wa/chats")
	if status != http.StatusOK {
		t.Fatalf("chats = %d %s", status, body)
	}
	if got := column(t, res, "last_message"); !slices.Equal(got, []any{"2026-10-30 09:00"}) {
		t.Errorf("last_message = %v", got)
	}
	if status, res, body = getTable(t, h, "/v1/wa/messages?chat=c1"); status != http.StatusOK || !slices.Equal(column(t, res, "body"), []any{"hi"}) {
		t.Errorf("messages = %d %s", status, body)
	}
}

// A connector's data.db appears when it first stores something; until then
// its source reads as empty.
func TestSourceAppearsOnceItsDatabaseExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "late", "data.db")
	h := newHandler(Config{
		Sources:      map[string]string{"late": path},
		ExposureFile: filepath.Join(dir, "exposure.toml"),
		Timeout:      2 * time.Second,
	}, testToken)

	if code, res, body := getTable(t, h, "/v1/late/chats"); code != http.StatusOK || len(res.Rows) != 0 {
		t.Fatalf("chats before the database exists: %d %s", code, body)
	}
	if code, _, _ := getTable(t, h, "/v1/late/messages?chat=c1"); code != http.StatusNotFound {
		t.Fatalf("messages before the database exists: %d", code)
	}

	mustDo(t, os.Mkdir(filepath.Dir(path), 0o750))
	st, err := store.Open(context.Background(), path)
	mustDo(t, err)
	mustDo(t, st.UpsertContainer(context.Background(), store.Container{ID: "c1", Kind: "dm", Name: "Ann"}))
	mustDo(t, st.UpsertMessage(context.Background(), store.Message{ID: "m1", ContainerID: "c1", SenderID: "u1", Body: "hi", SentAt: time.Now()}))
	mustDo(t, st.Close())

	if _, res, body := getTable(t, h, "/v1/late/messages?chat=c1"); len(res.Rows) != 1 {
		t.Fatalf("messages after the database exists: %s", body)
	}
}

func TestAuthAndBrokenExposure(t *testing.T) {
	t.Parallel()
	h, exposure := sourceHandler(t)
	for _, auth := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/whatsapp/chats", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: %d", auth, rec.Code)
		}
	}
	// A broken exposure file fails closed instead of serving unfiltered data.
	time.Sleep(10 * time.Millisecond)
	mustDo(t, os.WriteFile(exposure, []byte("[whatsap]\nhide_senders = []\n"), 0o600))
	if code, _, body := getTable(t, h, "/v1/whatsapp/chats"); code != http.StatusServiceUnavailable {
		t.Errorf("broken exposure: %d %s", code, body)
	}
}

func TestParseExposureStrict(t *testing.T) {
	t.Parallel()
	sources := []string{"discord", "whatsapp"}
	for name, conf := range map[string]string{
		"typo in key":          "[whatsapp]\nhide_sender = [\"+1\"]\n",
		"unconfigured source":  "[whatsap]\nhide_senders = [\"+1\"]\n",
		"top-level key":        "hide_senders = [\"+1\"]\n",
		"invalid auth pattern": "[auth]\nextra_body_patterns = [\"(\"]\n",
	} {
		if _, err := parseExposure(conf, sources); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	pol, err := parseExposure(testExposure+"[auth]\nextra_sender_patterns = [\"^bank$\"]\n", sources)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if got := pol.sources["discord"].HideContainers; !slices.Equal(got, []string{"g-work", "c-secret"}) {
		t.Errorf("discord hide_containers = %v", got)
	}
	if !pol.classifier.IsAuth("bank", "hello") {
		t.Error("[auth] patterns not applied")
	}
	if _, err := parseExposure("", nil); err != nil {
		t.Errorf("empty config rejected: %v", err)
	}
}

// Hidden IDs that match nothing stored are reported, as a typo or an ID in
// another form would hide nothing.
func TestUnmatchedHiddenIDs(t *testing.T) {
	t.Parallel()
	got, err := unmatched(fixture(t)["discord"], sourceExposure{
		HideContainers: []string{"g-work", "g-typo"},
		HideSenders:    []string{"u-hidden", "u1", "u-ghost"},
	})
	mustDo(t, err)
	if want := []miss{{"hide_containers", "g-typo"}, {"hide_senders", "u-ghost"}}; !slices.Equal(got, want) {
		t.Errorf("unmatched = %v, want %v", got, want)
	}
}

func TestFlagNames(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{
		"whatsapp=/var/lib/w/data.db": true,
		"auth=/x.db":                  false,
		"../x=/x.db":                  false,
		"proposals=/x.db":             false,
	} {
		if err := (sourceFlag{}).Set(v); (err == nil) != ok {
			t.Errorf("source %q: err = %v, want ok=%v", v, err, ok)
		}
	}
	for v, ok := range map[string]bool{
		"my-api2=/run/a.sock": true,
		"a/b=/run/x.sock":     false,
	} {
		if err := (connectorFlag{}).Set(v); (err == nil) != ok {
			t.Errorf("connector %q: err = %v, want ok=%v", v, err, ok)
		}
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
