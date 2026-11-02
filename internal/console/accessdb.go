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

// accessRequest is a stored access request. Extends is the grant an extend
// request would lengthen.
type accessRequest struct {
	proposal.AccessRequest
	ID, Status, GrantID  string
	Extends              string
	CreatedAt, ExpiresAt time.Time
	DecidedAt            time.Time
}

// accessChallenge is an access record waiting for the passkey: the exact
// bytes it signs, the access request a grant answers, the grant it
// replaces, and the names of its chats as the owner saw them.
type accessChallenge struct {
	Nonce    []byte
	Kind     string
	Record   []byte
	IssuedAt time.Time
	Session  []byte
	Request  string
	Replaces string
	Names    map[string]string
}

type agentRow struct {
	approval.Agent
	FirstSeen, LastSeen time.Time
}

// grantRow is a stored grant: its record, kept after the signature is
// dropped, and the chat names the owner saw when signing it.
type grantRow struct {
	approval.Grant
	Names      map[string]string
	RevokedAt  time.Time
	ReplacedBy string
}

func (d *db) addAccessChallenge(ctx context.Context, c accessChallenge) error {
	names, err := json.Marshal(c.Names)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `
		INSERT INTO access_challenges (nonce, kind, record, issued_at, session, request, replaces, names)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Nonce, c.Kind, c.Record, c.IssuedAt.UnixNano(), c.Session, c.Request, c.Replaces, string(names))
	return err
}

func (d *db) accessChallenge(ctx context.Context, nonce []byte) (accessChallenge, error) {
	c := accessChallenge{Nonce: nonce}
	var (
		issued int64
		names  string
	)
	err := d.sql.QueryRowContext(ctx,
		`SELECT kind, record, issued_at, session, request, replaces, names FROM access_challenges WHERE nonce = ?`, nonce).
		Scan(&c.Kind, &c.Record, &issued, &c.Session, &c.Request, &c.Replaces, &names)
	if errors.Is(err, sql.ErrNoRows) {
		return accessChallenge{}, errNoChallenge
	}
	if err != nil {
		return accessChallenge{}, err
	}
	c.IssuedAt = time.Unix(0, issued).UTC()
	return c, json.Unmarshal([]byte(names), &c.Names)
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

// addGrant stores g for a live agent. When c answers an access request it
// marks it granted while it is still pending; when c replaces a grant, the
// old one is revoked and replaced, once.
func (d *db) addGrant(ctx context.Context, c accessChallenge, g approval.Grant, signed []byte, now time.Time) error {
	names, err := json.Marshal(c.Names)
	if err != nil {
		return err
	}
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
		if c.Replaces != "" {
			res, err := tx.ExecContext(ctx, `
				UPDATE grants SET revoked_at = coalesce(revoked_at, ?), signed = NULL, replaced_by = ?
				WHERE id = ? AND replaced_by = ''`, ms(now), g.ID, c.Replaces)
			if err != nil {
				return err
			}
			if err := oneRow(res, errReplaced); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO grants (id, agent, connector, signed, record, names, created_at)
			SELECT ?, id, ?, ?, ?, ?, ? FROM agents WHERE id = ? AND deleted_at IS NULL`,
			g.ID, g.Connector, string(signed), c.Record, string(names), ms(g.CreatedAt), g.Agent)
		if err != nil {
			return err
		}
		return oneRow(res, errNoAgent)
	})
}

var errReplaced = errors.New("that grant was already extended or resumed")

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

// stopAll revokes every live grant and denies every waiting access request.
func (d *db) stopAll(ctx context.Context, now time.Time) (grants, requests int64, err error) {
	err = d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE grants SET revoked_at = ?, signed = NULL WHERE revoked_at IS NULL`, ms(now))
		if err != nil {
			return err
		}
		if grants, err = res.RowsAffected(); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE access_requests SET status = 'denied', decided_at = ? WHERE status = 'pending'`, ms(now))
		if err != nil {
			return err
		}
		requests, err = res.RowsAffected()
		return err
	})
	return grants, requests, err
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

// agents returns the live agents, oldest first.
func (d *db) agents(ctx context.Context) ([]agentRow, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, name, token_sha256, created_at, first_seen, last_seen
		FROM agents WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []agentRow
	for rs.Next() {
		var (
			a           agentRow
			created     int64
			first, last sql.NullInt64
		)
		if err := rs.Scan(&a.ID, &a.Name, &a.TokenSHA256, &created, &first, &last); err != nil {
			return nil, err
		}
		a.CreatedAt = fromMS(created)
		if first.Valid {
			a.FirstSeen, a.LastSeen = fromMS(first.Int64), fromMS(last.Int64)
		}
		out = append(out, a)
	}
	return out, rs.Err()
}

// seen records that agent used the gateway at at.
func (d *db) seen(ctx context.Context, agent string, at time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE agents SET first_seen = coalesce(first_seen, ?1), last_seen = max(coalesce(last_seen, 0), ?1)
		WHERE id = ?2 AND deleted_at IS NULL`, ms(at), agent)
	return err
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
func (d *db) grant(ctx context.Context, id string) (grantRow, error) {
	return d.oneGrant(ctx, `id = ? AND revoked_at IS NULL`, id)
}

// endedGrant returns a revoked grant that nothing replaced yet.
func (d *db) endedGrant(ctx context.Context, id string) (grantRow, error) {
	return d.oneGrant(ctx, `id = ? AND revoked_at IS NOT NULL AND replaced_by = ''`, id)
}

func (d *db) oneGrant(ctx context.Context, cond string, args ...any) (grantRow, error) {
	gs, err := d.queryGrants(ctx, cond, args...)
	if err != nil {
		return grantRow{}, err
	}
	if len(gs) == 0 {
		return grantRow{}, errNoGrant
	}
	return gs[0], nil
}

// grants returns the live grants, oldest first.
func (d *db) grants(ctx context.Context) ([]grantRow, error) {
	return d.queryGrants(ctx, `revoked_at IS NULL`)
}

// grantsSince returns the grants live now or revoked since since, oldest
// first.
func (d *db) grantsSince(ctx context.Context, since time.Time) ([]grantRow, error) {
	return d.queryGrants(ctx, `revoked_at IS NULL OR revoked_at >= ?`, ms(since))
}

// queryGrants decodes the grants matching cond from their records, oldest
// first; grants revoked before records were kept apart from signatures are
// left out.
func (d *db) queryGrants(ctx context.Context, cond string, args ...any) ([]grantRow, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT id, record, names, revoked_at, replaced_by FROM grants
		WHERE record IS NOT NULL AND (`+cond+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []grantRow
	for rs.Next() {
		var (
			id, names string
			record    []byte
			revoked   sql.NullInt64
			g         grantRow
		)
		if err := rs.Scan(&id, &record, &names, &revoked, &g.ReplacedBy); err != nil {
			return nil, err
		}
		if g.Grant, err = approval.DecodeGrant(record); err != nil {
			return nil, fmt.Errorf("grant %s: %w", id, err)
		}
		if err := json.Unmarshal([]byte(names), &g.Names); err != nil {
			return nil, fmt.Errorf("grant %s names: %w", id, err)
		}
		if revoked.Valid {
			g.RevokedAt = fromMS(revoked.Int64)
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
	var schedule any
	if r.Schedule != nil {
		b, err := json.Marshal(r.Schedule)
		if err != nil {
			return err
		}
		schedule = string(b)
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := accessQuota(ctx, tx, r.AgentID, r.CreatedAt, waiting, perHour); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO access_requests (id, agent_id, agent_name, connector, chats, unmatched, for_ns, horizon_ns, reason,
			                             schedule, extends, status, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			r.ID, r.AgentID, r.AgentName, r.Connector, string(chats), string(unmatched), int64(r.For), int64(r.Horizon), r.Reason,
			schedule, r.Extends, ms(r.CreatedAt), ms(r.ExpiresAt))
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

// accessRequestsBetween returns the requests made in [from, to).
func (d *db) accessRequestsBetween(ctx context.Context, from, to, now time.Time) ([]accessRequest, error) {
	return d.queryAccessRequests(ctx, now, `WHERE created_at >= ? AND created_at < ?`, ms(from), ms(to))
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
		SELECT id, agent_id, agent_name, connector, chats, unmatched, for_ns, horizon_ns, reason, schedule, extends,
		       status, grant_id, created_at, expires_at, decided_at
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
			schedule         sql.NullString
			forNS, horizonNS int64
			created, expires int64
			decided          sql.NullInt64
		)
		if err := rs.Scan(&r.ID, &r.AgentID, &r.AgentName, &r.Connector, &chats, &unmatched, &forNS, &horizonNS, &r.Reason,
			&schedule, &r.Extends, &r.Status, &r.GrantID, &created, &expires, &decided); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(chats), &r.Chats); err != nil {
			return nil, fmt.Errorf("access request %s: decode chats: %w", r.ID, err)
		}
		if err := json.Unmarshal([]byte(unmatched), &r.Unmatched); err != nil {
			return nil, fmt.Errorf("access request %s: decode unmatched: %w", r.ID, err)
		}
		if schedule.Valid {
			if err := json.Unmarshal([]byte(schedule.String), &r.Schedule); err != nil {
				return nil, fmt.Errorf("access request %s: decode schedule: %w", r.ID, err)
			}
		}
		r.Extend = r.Extends != ""
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
		INSERT INTO reads (at, agent_id, connector, verb, chat, chat_name, grants, count, dropped)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(e.At), e.AgentID, e.Connector, e.Verb, e.Chat, e.ChatName, string(grants), e.Count, e.Dropped)
	return err
}

// maxReads bounds the reads one Today page groups.
const maxReads = 20000

// reads returns the reads in [from, to), oldest first.
func (d *db) reads(ctx context.Context, from, to time.Time) ([]proposal.ReadEvent, error) {
	rs, err := d.sql.QueryContext(ctx, `
		SELECT at, agent_id, connector, verb, chat, chat_name, grants, count, dropped FROM reads
		WHERE at >= ? AND at < ? ORDER BY at LIMIT ?`, ms(from), ms(to), maxReads)
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
		if err := rs.Scan(&at, &e.AgentID, &e.Connector, &e.Verb, &e.Chat, &e.ChatName, &grants, &e.Count, &e.Dropped); err != nil {
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

// decidedBetween returns the proposals decided in [from, to).
func (d *db) decidedBetween(ctx context.Context, from, to time.Time) ([]row, error) {
	return d.query(ctx, `WHERE decided_at >= ? AND decided_at < ? ORDER BY decided_at`, ms(from), ms(to))
}
