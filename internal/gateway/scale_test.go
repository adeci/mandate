package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/adeci/mandate/store"
)

// A store the size of a real Discord account: 100 servers of 20 channels,
// 1,000 threads, 200,000 messages over 90 days. A grant on one server must
// list and search it well within the read timeout, as must the other
// grant shapes; per-message work over the whole store would not.
func TestReadsAtScale(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("builds a large store; the race detector slows SQLite tenfold")
	}
	path := filepath.Join(t.TempDir(), "data.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC()
	if _, err := st.DB().ExecContext(t.Context(), `
WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
INSERT INTO containers (id, kind, parent_id, name, updated_at)
SELECT 'guild:' || i, 'guild', NULL, 'server ' || i, ?1 FROM n WHERE i < 100
UNION ALL SELECT 'c' || i, 'channel', 'guild:' || (i % 100), '#channel-' || i, ?1 FROM n
UNION ALL SELECT 't' || i, 'thread', 'c' || (i * 2), 'thread ' || i, ?1 FROM n WHERE i < 1000`, now.Format(sentAt)); err != nil {
		t.Fatal(err)
	}
	// Two thirds of the messages in channels, the rest in threads, every
	// 50th mentioning a word to search for.
	if _, err := st.DB().ExecContext(t.Context(), `
WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 199999)
INSERT INTO messages (id, container_id, sender_id, sender_name, sent_at, body, ingested_at)
SELECT 'm' || i, iif(i % 3 = 2, 't' || (i % 1000), 'c' || (i % 2000)), 'u' || (i % 300), 'user',
       strftime('%Y-%m-%dT%H:%M:%fZ', ?1, '-' || (i * 39 % 7776000) || ' seconds'),
       iif(i % 50 = 7, 'see the rollout notes', 'message number ' || i), ?1
FROM n`, now.Format(sentAt)); err != nil {
		t.Fatal(err)
	}
	c := &conn{store: path}
	t.Cleanup(c.close)
	db, err := c.storeDB(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	week := now.Add(-7 * 24 * time.Hour).Format(sentAt)
	for name, scope := range map[string][]rule{
		"server, a week":   {{Chats: []string{"guild:7"}, Cutoff: week}},
		"server, all":      {{Chats: []string{"guild:7"}}},
		"channel, a week":  {{Chats: []string{"c7"}, Cutoff: week}},
		"every chat, week": {{Cutoff: week}},
	} {
		v, err := newView(db, neverShared{}, scope, now)
		if err != nil {
			t.Fatal(err)
		}
		for what, read := range map[string]func() (*table, error){
			"chats":        func() (*table, error) { return v.chats(t.Context(), "", 50) },
			"chats search": func() (*table, error) { return v.chats(t.Context(), "channel-7", 50) },
			"search":       func() (*table, error) { return v.search(t.Context(), "rollout", "", "", 0, 20) },
			"messages":     func() (*table, error) { return v.messages(t.Context(), "c7", 0, 20) },
		} {
			start := time.Now()
			tab, err := read()
			took := time.Since(start)
			switch {
			case err != nil:
				t.Errorf("%s, %s: %v", name, what, err)
			case len(tab.Rows) == 0:
				t.Errorf("%s, %s found nothing", name, what)
			}
			t.Logf("%s, %s: %v", name, what, took)
			if took > 500*time.Millisecond {
				t.Errorf("%s, %s took %v", name, what, took)
			}
		}
	}
}
