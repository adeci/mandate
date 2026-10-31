package approvals

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func (d *db) events(ctx context.Context) ([]event, error) {
	rs, err := d.sql.QueryContext(ctx, `SELECT event FROM events ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []event
	for rs.Next() {
		var data string
		if err := rs.Scan(&data); err != nil {
			return nil, err
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rs.Err()
}

func TestAuditTamperDetection(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, d *db, q string, args ...any) {
		t.Helper()
		if _, err := d.sql.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const forge = `replace(event, '"approved"', '"rejected"')`
	for _, tt := range []struct {
		name   string
		tamper func(t *testing.T, d *db)
		want   string
	}{
		{
			name:   "rewritten event",
			tamper: func(t *testing.T, d *db) { exec(t, d, `UPDATE events SET event = `+forge+` WHERE seq = 2`) },
			want:   "event 2 does not match its hash",
		},
		{
			// An attacker who knows the scheme fixes up the forged event's
			// hash; the next event still chains from the original.
			name: "rewritten event with its hash recomputed",
			tamper: func(t *testing.T, d *db) {
				var prev []byte
				var forged string
				if err := d.sql.QueryRowContext(ctx, `SELECT hash FROM events WHERE seq = 1`).Scan(&prev); err != nil {
					t.Fatal(err)
				}
				if err := d.sql.QueryRowContext(ctx, `SELECT `+forge+` FROM events WHERE seq = 2`).Scan(&forged); err != nil {
					t.Fatal(err)
				}
				exec(t, d, `UPDATE events SET event = ?, hash = ? WHERE seq = 2`, forged, chainHash(prev, []byte(forged)))
			},
			want: "event 3 does not match its hash",
		},
		{
			name:   "deleted event",
			tamper: func(t *testing.T, d *db) { exec(t, d, `DELETE FROM events WHERE seq = 2`) },
			want:   "event 2 is missing",
		},
		{
			name:   "renumbered event",
			tamper: func(t *testing.T, d *db) { exec(t, d, `UPDATE events SET seq = 10 WHERE seq = 3`) },
			want:   "event 3 is missing",
		},
		{
			name: "copied hash",
			tamper: func(t *testing.T, d *db) {
				exec(t, d, `UPDATE events SET hash = (SELECT hash FROM events WHERE seq = 1) WHERE seq = 2`)
			},
			want: "event 2 does not match its hash",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t)
			p := h.submit(t, "running late")
			if status, _ := h.approve(t, p.ID, h.key.Assert(h.challenge(t, p.ID, nil))); status != http.StatusOK {
				t.Fatalf("approve = %d", status)
			}
			d := h.srv.db
			if err := d.verifyAudit(ctx); err != nil {
				t.Fatalf("untampered log: %v", err)
			}
			if _, err := d.sql.ExecContext(ctx, `UPDATE events SET event = event WHERE seq = 1`); err == nil {
				t.Fatal("events table accepted an update")
			}
			if _, err := d.sql.ExecContext(ctx, `DELETE FROM events WHERE seq = 1`); err == nil {
				t.Fatal("events table accepted a delete")
			}

			// Someone with write access to the file drops the guards first.
			exec(t, d, `DROP TRIGGER events_no_update`)
			exec(t, d, `DROP TRIGGER events_no_delete`)
			tt.tamper(t, d)
			err := d.verifyAudit(ctx)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("verify = %v, want %q", err, tt.want)
			}
		})
	}
}
