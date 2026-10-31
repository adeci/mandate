package console

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // registers "sqlite3"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

//go:embed schema.sql
var schema string

var (
	errNotFound   = errors.New("no such request")
	errNotPending = errors.New("request is no longer pending")
	errTooMany    = errors.New("too many pending requests")
)

type row struct {
	proposal.Proposal
	Args      map[string]string
	DecidedAt time.Time
	DecidedBy string
}

// challenge is one outstanding approval challenge: the exact record bytes the
// passkey signs, with the nonce and issue time that go into the hash.
type challenge struct {
	Nonce    []byte
	Proposal string
	Record   []byte
	IssuedAt time.Time
	Session  []byte
}

type passkey struct {
	approval.Approver
	Synced     bool
	EnrolledAt time.Time
}

type db struct {
	sql *sql.DB
}

func openDB(ctx context.Context, path string) (*db, error) {
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
	// at-most-once claim rely on.
	s.SetMaxOpenConns(1)
	if _, err := s.ExecContext(ctx, schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &db{sql: s}, nil
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
		return err
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

func (d *db) decided(ctx context.Context, limit int) ([]row, error) {
	return d.query(ctx, `WHERE status != 'pending' ORDER BY decided_at DESC LIMIT ?`, limit)
}

func (d *db) pendingCount(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM proposals WHERE status = 'pending'`).Scan(&n)
	return n, err
}

func (d *db) query(ctx context.Context, where string, args ...any) ([]row, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, connector, op, args, display, note, status, result,
		       created_at, expires_at, decided_at, decided_by
		FROM proposals `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var (
			r                 row
			argsJSON, display string
			result, by        sql.NullString
			created, expires  int64
			decided           sql.NullInt64
			status            string
		)
		if err := rs.Scan(&r.ID, &r.Connector, &r.Op, &argsJSON, &display, &r.Note, &status, &result,
			&created, &expires, &decided, &by); err != nil {
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
		if decided.Valid {
			r.DecidedAt = fromMS(decided.Int64)
		}
		r.DecidedBy = by.String
		out = append(out, r)
	}
	return out, rs.Err()
}

func (d *db) addChallenge(ctx context.Context, c challenge, now time.Time) error {
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO challenges (nonce, proposal, record, issued_at, session)
		SELECT ?, id, ?, ?, ? FROM proposals WHERE id = ? AND status = 'pending' AND expires_at > ?`,
		c.Nonce, c.Record, c.IssuedAt.UnixNano(), c.Session, c.Proposal, ms(now))
	if err != nil {
		return err
	}
	return oneRow(res, errNotPending)
}

func (d *db) challenge(ctx context.Context, nonce []byte) (challenge, error) {
	c := challenge{Nonce: nonce}
	var issued int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT proposal, record, issued_at, session FROM challenges WHERE nonce = ?`, nonce).
		Scan(&c.Proposal, &c.Record, &issued, &c.Session)
	if errors.Is(err, sql.ErrNoRows) {
		return challenge{}, errNotFound
	}
	c.IssuedAt = time.Unix(0, issued).UTC()
	return c, err
}

// claim spends challenge c and moves its proposal from pending to executing,
// storing the proof. Exactly one caller wins; everyone else gets
// errNotPending, which is what keeps a double tap from executing twice.
func (d *db) claim(ctx context.Context, c challenge, rec approval.Record, assertion json.RawMessage, approver string, now time.Time) error {
	args, err := json.Marshal(rec.Args)
	if err != nil {
		return err
	}
	display, err := json.Marshal(rec.Display)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM challenges WHERE nonce = ? AND proposal = ?`, c.Nonce, c.Proposal)
		if err != nil {
			return err
		}
		if err := oneRow(res, errNotPending); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE proposals SET status = 'executing', args = ?, display = ?, decided_at = ?, decided_by = ?,
			       record = ?, nonce = ?, issued_at = ?, assertion = ?
			WHERE id = ? AND status = 'pending' AND expires_at > ?`,
			string(args), string(display), ms(now), approver,
			c.Record, c.Nonce, c.IssuedAt.UnixNano(), string(assertion), c.Proposal, ms(now))
		if err != nil {
			return err
		}
		if err := oneRow(res, errNotPending); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM challenges WHERE proposal = ?`, c.Proposal)
		return err
	})
}

func (d *db) finish(ctx context.Context, id string, res connector.Result) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	r, err := d.sql.ExecContext(ctx,
		`UPDATE proposals SET status = ?, result = ? WHERE id = ? AND status = 'executing'`,
		string(res.Status), string(b), id)
	if err != nil {
		return err
	}
	return oneRow(r, errNotPending)
}

func (d *db) reject(ctx context.Context, id, by string, now time.Time) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE proposals SET status = 'rejected', decided_at = ?, decided_by = ?
			WHERE id = ? AND status = 'pending' AND expires_at > ?`, ms(now), by, id, ms(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			_, err = tx.ExecContext(ctx, `DELETE FROM challenges WHERE proposal = ?`, id)
			return err
		}
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM proposals WHERE id = ?`, id).Scan(&exists)
		switch {
		case err != nil:
			return err
		case exists == 0:
			return errNotFound
		default:
			return errNotPending
		}
	})
}

func (d *db) expireDue(ctx context.Context, now time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE proposals SET status = 'expired', decided_at = expires_at
		WHERE status = 'pending' AND expires_at <= ?`, ms(now))
	return err
}

// sweep expires proposals and deletes challenges too old to verify and
// sessions past their lifetime.
func (d *db) sweep(ctx context.Context, now time.Time) error {
	if err := d.expireDue(ctx, now); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `
		DELETE FROM challenges WHERE issued_at < ?
		   OR proposal NOT IN (SELECT id FROM proposals WHERE status = 'pending')`,
		now.Add(-approval.MaxAge-time.Minute).UnixNano())
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, ms(now))
	return err
}

// recoverInterrupted settles proposals that were executing when the process
// stopped. The connector may have run them, so they become unknown, never
// pending again.
func (d *db) recoverInterrupted(ctx context.Context) ([]string, error) {
	res, err := json.Marshal(connector.Result{Status: connector.Unknown, Detail: "interrupted while executing"})
	if err != nil {
		return nil, err
	}
	rs, err := d.sql.QueryContext(ctx,
		`UPDATE proposals SET status = 'unknown', result = ? WHERE status = 'executing' RETURNING id`, string(res))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var ids []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rs.Err()
}

func (d *db) createSession(ctx context.Context, hash []byte, credentialID string, now, expires time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO sessions (hash, credential_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, credentialID, ms(now), ms(expires))
	return err
}

func (d *db) session(ctx context.Context, hash []byte, now time.Time) (string, error) {
	var id string
	err := d.sql.QueryRowContext(ctx,
		`SELECT credential_id FROM sessions WHERE hash = ? AND expires_at > ?`, hash, ms(now)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return id, err
}

func (d *db) deleteSession(ctx context.Context, hash []byte) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, hash)
	return err
}

func (d *db) addPasskey(ctx context.Context, p passkey) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO passkeys (credential_id, name, public_key, synced, enrolled_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (credential_id) DO UPDATE SET name = excluded.name, public_key = excluded.public_key,
		    synced = excluded.synced, enrolled_at = excluded.enrolled_at`,
		p.CredentialID, p.Name, p.PublicKey, p.Synced, ms(p.EnrolledAt))
	return err
}

func (d *db) passkeys(ctx context.Context) ([]passkey, error) {
	rs, err := d.sql.QueryContext(ctx,
		`SELECT credential_id, name, public_key, synced, enrolled_at FROM passkeys ORDER BY enrolled_at`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []passkey
	for rs.Next() {
		var (
			p  passkey
			at int64
		)
		if err := rs.Scan(&p.CredentialID, &p.Name, &p.PublicKey, &p.Synced, &at); err != nil {
			return nil, err
		}
		p.EnrolledAt = fromMS(at)
		out = append(out, p)
	}
	return out, rs.Err()
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
