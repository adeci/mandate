package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/internal/proposal"
)

// accessKept is how long reads and decided access requests are kept.
const accessKept = 30 * 24 * time.Hour

var (
	errNoAgent     = errors.New("no such agent, or it was deleted")
	errNoGrant     = errors.New("no such grant, or it was revoked")
	errNoChallenge = errors.New("this approval was already used or has expired; start again")
)

// accessRequest is a stored access request.
type accessRequest struct {
	proposal.AccessRequest
	ID, Status, GrantID  string
	CreatedAt, ExpiresAt time.Time
	DecidedAt            time.Time
}

// accessChallenge is an access record waiting for the passkey: the exact
// bytes it signs, and the access request a grant answers.
type accessChallenge struct {
	Nonce    []byte
	Kind     string
	Record   []byte
	IssuedAt time.Time
	Session  []byte
	Request  string
}

type agentRow struct {
	approval.Agent
	LastRead time.Time
}

func (d *db) addAccessChallenge(ctx context.Context, c accessChallenge) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO access_challenges (nonce, kind, record, issued_at, session, request)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.Nonce, c.Kind, c.Record, c.IssuedAt.UnixNano(), c.Session, c.Request)
	return err
}

func (d *db) accessChallenge(ctx context.Context, nonce []byte) (accessChallenge, error) {
	c := accessChallenge{Nonce: nonce}
	var issued int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT kind, record, issued_at, session, request FROM access_challenges WHERE nonce = ?`, nonce).
		Scan(&c.Kind, &c.Record, &issued, &c.Session, &c.Request)
	if errors.Is(err, sql.ErrNoRows) {
		return accessChallenge{}, errNoChallenge
	}
	c.IssuedAt = time.Unix(0, issued).UTC()
	return c, err
}

// spend deletes challenge c and runs store in the same transaction, so a
// challenge stores at most one record.
func (d *db) spend(ctx context.Context, c accessChallenge, store func(*sql.Tx) error) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM access_challenges WHERE nonce = ?`, c.Nonce)
		if err != nil {
			return err
		}
		if err := oneRow(res, errNoChallenge); err != nil {
			return err
		}
		return store(tx)
	})
}

func (d *db) addAgent(ctx context.Context, c accessChallenge, a approval.Agent, signed []byte) error {
	return d.spend(ctx, c, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO agents (id, name, token_sha256, signed, created_at) VALUES (?, ?, ?, ?, ?)`,
			a.ID, a.Name, a.TokenSHA256, string(signed), ms(a.CreatedAt))
		return err
	})
}

// addGrant stores g for a live agent and, when c answers an access request,
// marks it granted while it is still pending.
func (d *db) addGrant(ctx context.Context, c accessChallenge, g approval.Grant, signed []byte, now time.Time) error {
	return d.spend(ctx, c, func(tx *sql.Tx) error {
		if c.Request != "" {
			res, err := tx.ExecContext(ctx, `
				UPDATE access_requests SET status = 'granted', grant_id = ?, decided_at = ?
				WHERE id = ? AND status = 'pending' AND expires_at > ?`, g.ID, ms(now), c.Request, ms(now))
			if err != nil {
				return err
			}
			if err := oneRow(res, errNotPending); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO grants (id, agent, connector, signed, created_at)
			SELECT ?, id, ?, ?, ? FROM agents WHERE id = ? AND deleted_at IS NULL`,
			g.ID, g.Connector, string(signed), ms(g.CreatedAt), g.Agent)
		if err != nil {
			return err
		}
		return oneRow(res, errNoAgent)
	})
}

// deleteAgent ends the agent and its grants, dropping their signatures, and
// refuses its waiting access requests and proposals.
func (d *db) deleteAgent(ctx context.Context, id string, now time.Time) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE agents SET deleted_at = ?, signed = NULL WHERE id = ? AND deleted_at IS NULL`, ms(now), id)
		if err != nil {
			return err
		}
		if err := oneRow(res, errNoAgent); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE grants SET revoked_at = coalesce(revoked_at, ?), signed = NULL WHERE agent = ?`, ms(now), id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE access_requests SET status = 'denied', decided_at = ?
			WHERE agent_id = ? AND status = 'pending'`, ms(now), id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE proposals SET status = 'rejected', decided_at = ?, decided_by = 'agent deleted'
			WHERE agent_id = ? AND status = 'pending'`, ms(now), id)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			DELETE FROM challenges WHERE proposal IN (SELECT id FROM proposals WHERE agent_id = ? AND status != 'pending')`, id)
		return err
	})
}

func (d *db) revokeGrant(ctx context.Context, id string, now time.Time) error {
	res, err := d.sql.ExecContext(ctx, `UPDATE grants SET revoked_at = ?, signed = NULL WHERE id = ? AND revoked_at IS NULL`, ms(now), id)
	if err != nil {
		return err
	}
	return oneRow(res, errNoGrant)
}

// liveRecords returns the signed agents and grants not deleted or revoked,
// as stored.
func (d *db) liveRecords(ctx context.Context) ([]json.RawMessage, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT signed FROM agents WHERE deleted_at IS NULL AND signed IS NOT NULL
		UNION ALL
		SELECT signed FROM grants WHERE revoked_at IS NULL AND signed IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []json.RawMessage{}
	for rs.Next() {
		var s string
		if err := rs.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(s))
	}
	return out, rs.Err()
}

// agents returns the live agents, oldest first, with their last read.
func (d *db) agents(ctx context.Context) ([]agentRow, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, name, token_sha256, created_at, (SELECT max(at) FROM reads WHERE agent_id = agents.id)
		FROM agents WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []agentRow
	for rs.Next() {
		var (
			a       agentRow
			created int64
			last    sql.NullInt64
		)
		if err := rs.Scan(&a.ID, &a.Name, &a.TokenSHA256, &created, &last); err != nil {
			return nil, err
		}
		a.CreatedAt = fromMS(created)
		if last.Valid {
			a.LastRead = fromMS(last.Int64)
		}
		out = append(out, a)
	}
	return out, rs.Err()
}

func (d *db) agent(ctx context.Context, id string) (approval.Agent, error) {
	var (
		a       approval.Agent
		created int64
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT id, name, token_sha256, created_at FROM agents WHERE id = ? AND deleted_at IS NULL`, id).
		Scan(&a.ID, &a.Name, &a.TokenSHA256, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return approval.Agent{}, errNoAgent
	}
	a.CreatedAt = fromMS(created)
	return a, err
}

// grant returns a live grant by ID.
func (d *db) grant(ctx context.Context, id string) (approval.Grant, error) {
	gs, err := d.queryGrants(ctx, `WHERE id = ? AND revoked_at IS NULL`, id)
	if err != nil {
		return approval.Grant{}, err
	}
	if len(gs) == 0 {
		return approval.Grant{}, errNoGrant
	}
	return gs[0], nil
}

// grants returns the live grants, oldest first.
func (d *db) grants(ctx context.Context) ([]approval.Grant, error) {
	return d.queryGrants(ctx, `WHERE revoked_at IS NULL ORDER BY created_at, id`)
}

// queryGrants decodes grants from their signed records.
func (d *db) queryGrants(ctx context.Context, where string, args ...any) ([]approval.Grant, error) {
	rs, err := d.sql.QueryContext(ctx, `SELECT id, signed FROM grants `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []approval.Grant
	for rs.Next() {
		var (
			id, signed string
			s          approval.Signed
		)
		if err := rs.Scan(&id, &signed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(signed), &s); err != nil {
			return nil, fmt.Errorf("grant %s: %w", id, err)
		}
		g, err := approval.DecodeGrant(s.Record)
		if err != nil {
			return nil, fmt.Errorf("grant %s: %w", id, err)
		}
		out = append(out, g)
	}
	return out, rs.Err()
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// accessQuota is errTooMany when agent has waiting requests pending, or
// made perHour in the hour before now.
func accessQuota(ctx context.Context, q rowQuerier, agent string, now time.Time, waiting, perHour int) error {
	var pending, recent int
	err := q.QueryRowContext(ctx, `
		SELECT coalesce(sum(status = 'pending' AND expires_at > ?2), 0), coalesce(sum(created_at > ?3), 0)
		FROM access_requests WHERE agent_id = ?1`,
		agent, ms(now), ms(now.Add(-time.Hour))).Scan(&pending, &recent)
	if err != nil {
		return err
	}
	if pending >= waiting || recent >= perHour {
		return errTooMany
	}
	return nil
}

func (d *db) accessQuota(ctx context.Context, agent string, now time.Time, waiting, perHour int) error {
	return accessQuota(ctx, d.sql, agent, now, waiting, perHour)
}

// addAccessRequest stores r unless its agent is over accessQuota.
func (d *db) addAccessRequest(ctx context.Context, r accessRequest, waiting, perHour int) error {
	chats, err := json.Marshal(r.Chats)
	if err != nil {
		return err
	}
	unmatched, err := json.Marshal(r.Unmatched)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := accessQuota(ctx, tx, r.AgentID, r.CreatedAt, waiting, perHour); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO access_requests (id, agent_id, agent_name, connector, chats, unmatched, for_ns, horizon_ns, reason,
			                             status, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			r.ID, r.AgentID, r.AgentName, r.Connector, string(chats), string(unmatched), int64(r.For), int64(r.Horizon), r.Reason,
			ms(r.CreatedAt), ms(r.ExpiresAt))
		return err
	})
}

func (d *db) accessRequest(ctx context.Context, id string, now time.Time) (accessRequest, error) {
	rs, err := d.queryAccessRequests(ctx, now, `WHERE id = ?`, id)
	if err != nil {
		return accessRequest{}, err
	}
	if len(rs) == 0 {
		return accessRequest{}, errNotFound
	}
	return rs[0], nil
}

func (d *db) pendingAccessRequests(ctx context.Context, now time.Time) ([]accessRequest, error) {
	return d.queryAccessRequests(ctx, now, `WHERE status = 'pending' ORDER BY created_at DESC`)
}

func (d *db) pendingAccessCount(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM access_requests WHERE status = 'pending' AND expires_at > ?`, ms(now)).Scan(&n)
	return n, err
}

// queryAccessRequests expires due requests first, so none is pending past
// its time.
func (d *db) queryAccessRequests(ctx context.Context, now time.Time, where string, args ...any) ([]accessRequest, error) {
	if err := d.expireAccessRequests(ctx, now); err != nil {
		return nil, err
	}
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, agent_id, agent_name, connector, chats, unmatched, for_ns, horizon_ns, reason, status, grant_id,
		       created_at, expires_at, decided_at
		FROM access_requests `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []accessRequest
	for rs.Next() {
		var (
			r                accessRequest
			chats, unmatched string
			forNS, horizonNS int64
			created, expires int64
			decided          sql.NullInt64
		)
		if err := rs.Scan(&r.ID, &r.AgentID, &r.AgentName, &r.Connector, &chats, &unmatched, &forNS, &horizonNS, &r.Reason,
			&r.Status, &r.GrantID, &created, &expires, &decided); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(chats), &r.Chats); err != nil {
			return nil, fmt.Errorf("access request %s: decode chats: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(unmatched), &r.Unmatched); err != nil {
			return nil, fmt.Errorf("access request %s: decode unmatched: %w", r.ID, err)
		}
		r.For, r.Horizon = time.Duration(forNS), time.Duration(horizonNS)
		r.CreatedAt, r.ExpiresAt = fromMS(created), fromMS(expires)
		if decided.Valid {
			r.DecidedAt = fromMS(decided.Int64)
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func (d *db) denyAccessRequest(ctx context.Context, id string, now time.Time) error {
	res, err := d.sql.ExecContext(ctx, `
		UPDATE access_requests SET status = 'denied', decided_at = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ?`, ms(now), id, ms(now))
	if err != nil {
		return err
	}
	return oneRow(res, errNotPending)
}

func (d *db) expireAccessRequests(ctx context.Context, now time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE access_requests SET status = 'expired', decided_at = expires_at
		WHERE status = 'pending' AND expires_at <= ?`, ms(now))
	return err
}

func (d *db) addRead(ctx context.Context, e proposal.ReadEvent) error {
	grants, err := json.Marshal(e.Grants)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO reads (at, agent_id, connector, verb, chat, grants, count, dropped) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(e.At), e.AgentID, e.Connector, e.Verb, e.Chat, string(grants), e.Count, e.Dropped)
	return err
}

// reads returns the newest limit reads.
func (d *db) reads(ctx context.Context, limit int) ([]proposal.ReadEvent, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT at, agent_id, connector, verb, chat, grants, count, dropped FROM reads ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []proposal.ReadEvent
	for rs.Next() {
		var (
			e      proposal.ReadEvent
			at     int64
			grants string
		)
		if err := rs.Scan(&at, &e.AgentID, &e.Connector, &e.Verb, &e.Chat, &grants, &e.Count, &e.Dropped); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(grants), &e.Grants); err != nil {
			return nil, fmt.Errorf("decode read grants: %w", err)
		}
		e.At = fromMS(at)
		out = append(out, e)
	}
	return out, rs.Err()
}

// sweepAccess expires access requests, and deletes challenges too old to
// approve and reads and decided requests older than accessKept.
func (d *db) sweepAccess(ctx context.Context, now time.Time) error {
	if err := d.expireAccessRequests(ctx, now); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `DELETE FROM access_challenges WHERE issued_at < ?`,
		now.Add(-approval.MaxAge-time.Minute).UnixNano())
	if err != nil {
		return err
	}
	old := ms(now.Add(-accessKept))
	if _, err = d.sql.ExecContext(ctx, `DELETE FROM access_requests WHERE status != 'pending' AND decided_at < ?`, old); err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `DELETE FROM reads WHERE at < ?`, old)
	return err
}
