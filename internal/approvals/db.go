package approvals

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers "sqlite3"

	"github.com/adeci/mandate/internal/approval"
	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/proposal"
)

//go:embed schema.sql
var schema string

var (
	errNotFound   = errors.New("no such proposal")
	errNotPending = errors.New("proposal is no longer pending")
	errTooMany    = errors.New("too many pending proposals")
)

// Audit event kinds.
const (
	evProposed = "proposed"
	evEdited   = "edited"
	evApproved = "approved"
	evRejected = "rejected"
	evExecuted = "executed"
	evExpired  = "expired"
	evEnrolled = "enrolled"
)

type row struct {
	proposal.Proposal
	Args     map[string]string
	Record   []byte
	Nonce    []byte
	IssuedAt time.Time
}

type db struct {
	sql *sql.DB
	log *slog.Logger
}

func openDB(ctx context.Context, path string, log *slog.Logger) (*db, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(DELETE)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	s, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: transactions serialize, which the pending cap and the
	// audit chain rely on.
	s.SetMaxOpenConns(1)
	if _, err := s.ExecContext(ctx, schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &db{sql: s, log: log}, nil
}

func (d *db) Close() error { return d.sql.Close() }

func (d *db) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// insert stores a new pending proposal unless limit are already pending.
func (d *db) insert(ctx context.Context, r row, limit int) error {
	args, err := json.Marshal(r.Args)
	if err != nil {
		return err
	}
	display, err := json.Marshal(r.Display)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		var n int
		err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM proposals WHERE status = 'pending' AND expires_at > ?`,
			ms(r.CreatedAt)).Scan(&n)
		if err != nil {
			return err
		}
		if n >= limit {
			return errTooMany
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO proposals (id, connector, op, args, display, note, status, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			r.ID, r.Connector, r.Op, string(args), string(display), r.Note, ms(r.CreatedAt), ms(r.ExpiresAt))
		if err != nil {
			return err
		}
		return d.appendEvent(ctx, tx, r.CreatedAt, r.ID, evProposed,
			map[string]string{"connector": r.Connector, "op": r.Op})
	})
}

func (d *db) get(ctx context.Context, id string) (row, error) {
	rows, err := d.query(ctx, `WHERE id = ?`, id)
	if err != nil {
		return row{}, err
	}
	if len(rows) == 0 {
		return row{}, errNotFound
	}
	return rows[0], nil
}

func (d *db) pending(ctx context.Context) ([]row, error) {
	return d.query(ctx, `WHERE status = 'pending' ORDER BY created_at DESC`)
}

func (d *db) query(ctx context.Context, where string, args ...any) ([]row, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, connector, op, args, display, note, status, result,
		       created_at, expires_at, record, nonce, issued_at
		FROM proposals `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var (
			r                  row
			argsJSON, display  string
			result             sql.NullString
			created, expires   int64
			issued             sql.NullInt64
			status             string
			record, nonceBytes []byte
		)
		if err := rs.Scan(&r.ID, &r.Connector, &r.Op, &argsJSON, &display, &r.Note, &status, &result,
			&created, &expires, &record, &nonceBytes, &issued); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(argsJSON), &r.Args); err != nil {
			return nil, fmt.Errorf("proposal %s: decode args: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(display), &r.Display); err != nil {
			return nil, fmt.Errorf("proposal %s: decode display: %w", r.ID, err)
		}
		if result.Valid {
			r.Result = new(connector.Result)
			if err := json.Unmarshal([]byte(result.String), r.Result); err != nil {
				return nil, fmt.Errorf("proposal %s: decode result: %w", r.ID, err)
			}
		}
		r.Status = proposal.Status(status)
		r.CreatedAt, r.ExpiresAt = fromMS(created), fromMS(expires)
		r.Record, r.Nonce = record, nonceBytes
		if issued.Valid {
			r.IssuedAt = time.Unix(0, issued.Int64).UTC()
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// setChallenge replaces the outstanding challenge of a pending proposal.
func (d *db) setChallenge(ctx context.Context, id string, record, nonce []byte, issuedAt time.Time, edited []string) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE proposals SET record = ?, nonce = ?, issued_at = ?
			WHERE id = ? AND status = 'pending' AND expires_at > ?`,
			record, nonce, issuedAt.UnixNano(), id, ms(issuedAt))
		if err != nil {
			return err
		}
		if err := oneRow(res, errNotPending); err != nil {
			return err
		}
		if len(edited) == 0 {
			return nil
		}
		return d.appendEvent(ctx, tx, issuedAt, id, evEdited, map[string]any{"params": edited})
	})
}

// claim moves a pending proposal to executing, but only while nonce is still
// its outstanding challenge. Exactly one caller wins; everyone else gets
// errNotPending, which is what keeps a double tap from executing twice.
func (d *db) claim(ctx context.Context, id string, nonce []byte, rec approval.Record, approver string, now time.Time) error {
	args, err := json.Marshal(rec.Args)
	if err != nil {
		return err
	}
	display, err := json.Marshal(rec.Display)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE proposals SET status = 'executing', args = ?, display = ?
			WHERE id = ? AND status = 'pending' AND nonce = ? AND expires_at > ?`,
			string(args), string(display), id, nonce, ms(now))
		if err != nil {
			return err
		}
		if err := oneRow(res, errNotPending); err != nil {
			return err
		}
		var record []byte
		if err := tx.QueryRowContext(ctx, `SELECT record FROM proposals WHERE id = ?`, id).Scan(&record); err != nil {
			return err
		}
		sum := sha256.Sum256(record)
		return d.appendEvent(ctx, tx, now, id, evApproved,
			map[string]string{"approver": approver, "record_sha256": hex.EncodeToString(sum[:])})
	})
}

// finish stores the result of an executing proposal.
func (d *db) finish(ctx context.Context, id string, res connector.Result, now time.Time) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			`UPDATE proposals SET status = ?, result = ? WHERE id = ? AND status = 'executing'`,
			string(res.Status), string(b), id)
		if err != nil {
			return err
		}
		if err := oneRow(r, errNotPending); err != nil {
			return err
		}
		return d.appendEvent(ctx, tx, now, id, evExecuted, res)
	})
}

func (d *db) reject(ctx context.Context, id string, now time.Time) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE proposals SET status = 'rejected' WHERE id = ? AND status = 'pending'`, id)
		if err != nil {
			return err
		}
		if err := oneRow(res, errNotPending); err != nil {
			return err
		}
		return d.appendEvent(ctx, tx, now, id, evRejected, nil)
	})
}

// expireDue marks pending proposals past their expiry as expired.
func (d *db) expireDue(ctx context.Context, now time.Time) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		rs, err := tx.QueryContext(ctx,
			`SELECT id FROM proposals WHERE status = 'pending' AND expires_at <= ? ORDER BY created_at`, ms(now))
		if err != nil {
			return err
		}
		var ids []string
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				rs.Close()
				return err
			}
			ids = append(ids, id)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE proposals SET status = 'expired' WHERE id = ?`, id); err != nil {
				return err
			}
			if err := d.appendEvent(ctx, tx, now, id, evExpired, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// recoverInterrupted settles proposals that were executing when the process
// stopped. The connector may have sent them, so they become unknown, never
// pending again.
func (d *db) recoverInterrupted(ctx context.Context, now time.Time) error {
	rows, err := d.query(ctx, `WHERE status = 'executing'`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		res := connector.Result{Status: connector.Unknown, Detail: "interrupted while executing"}
		if err := d.finish(ctx, r.ID, res, now); err != nil {
			return err
		}
		d.log.Warn("proposal was interrupted while executing", "id", r.ID)
	}
	return nil
}

// note appends an event that belongs to no proposal state change.
func (d *db) note(ctx context.Context, at time.Time, kind string, detail any) error {
	return d.tx(ctx, func(tx *sql.Tx) error { return d.appendEvent(ctx, tx, at, "", kind, detail) })
}

func oneRow(res sql.Result, none error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return none
	}
	return nil
}

// event is one audit entry. Its JSON encoding is what the chain hashes, and
// it is stored verbatim.
type event struct {
	Seq      int64           `json:"seq"`
	At       time.Time       `json:"at"`
	Proposal string          `json:"proposal,omitempty"`
	Kind     string          `json:"kind"`
	Detail   json.RawMessage `json:"detail,omitempty"`
}

func chainHash(prev, ev []byte) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(ev)
	return h.Sum(nil)
}

// appendEvent must be the last statement of its transaction, so the hash it
// logs is the one that gets committed.
func (d *db) appendEvent(ctx context.Context, tx *sql.Tx, at time.Time, id, kind string, detail any) error {
	var (
		seq  int64
		prev []byte
	)
	err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		prev = make([]byte, sha256.Size)
	case err != nil:
		return err
	}
	ev := event{Seq: seq + 1, At: at.UTC(), Proposal: id, Kind: kind}
	if detail != nil {
		if ev.Detail, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	hash := chainHash(prev, b)
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (seq, event, hash) VALUES (?, ?, ?)`,
		ev.Seq, string(b), hash); err != nil {
		return err
	}
	// The journal keeps a copy of each hash outside the database, which is
	// what exposes a rewritten chain or a truncated tail.
	d.log.Info("audit", "seq", ev.Seq, "kind", kind, "proposal", id, "hash", hex.EncodeToString(hash))
	return nil
}

// verifyAudit walks the event chain and reports the first event whose
// content, position, or hash doesn't match. It can't detect removal of the
// newest events; compare the last hash with the journal for that.
func (d *db) verifyAudit(ctx context.Context) error {
	rs, err := d.sql.QueryContext(ctx, `SELECT seq, event, hash FROM events ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rs.Close()
	prev := make([]byte, sha256.Size)
	want := int64(1)
	for rs.Next() {
		var (
			seq  int64
			data string
			hash []byte
		)
		if err := rs.Scan(&seq, &data, &hash); err != nil {
			return err
		}
		if seq != want {
			return fmt.Errorf("audit event %d is missing", want)
		}
		var ev event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("audit event %d: decode: %w", seq, err)
		}
		if ev.Seq != seq {
			return fmt.Errorf("audit event %d claims sequence %d", seq, ev.Seq)
		}
		if !bytes.Equal(hash, chainHash(prev, []byte(data))) {
			return fmt.Errorf("audit event %d does not match its hash", seq)
		}
		prev = hash
		want++
	}
	return rs.Err()
}
