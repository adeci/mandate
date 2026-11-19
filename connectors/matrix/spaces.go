package main

import (
	"context"
	"slices"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	kindSpace = "space"

	// The spaces the owner joined list their rooms here, from m.space.child.
	// Sync sends those only when they change, so a store synced before they
	// were kept gets them from one sync with every room's full state, which
	// leaves a row in matrix_full_state.
	spacesSchema = `
CREATE TABLE IF NOT EXISTS matrix_space_children (
    space_id TEXT NOT NULL,
    child_id TEXT NOT NULL,
    PRIMARY KEY (space_id, child_id)
);
CREATE TABLE IF NOT EXISTS matrix_full_state (done INTEGER NOT NULL)`

	// The three ancestors the store allows.
	maxDepth = 4
)

func (a *account) isSpace(ctx context.Context, room id.RoomID) bool {
	cr, err := a.state.GetCreate(ctx, room)
	return err == nil && cr != nil && cr.Content.AsCreate().Type == event.RoomTypeSpace
}

// spaceChild records or removes a space's child. A child without via is
// removed, and so is a room the owner is in when the space's sender could
// not have named the space its parent: a space's admins cannot pull in a
// room of the owner's, and with it change what a grant or Never share on
// the space covers.
func (c *mxConnector) spaceChild(ctx context.Context, a *account, evt *event.Event) error {
	space, child := evt.RoomID.String(), *evt.StateKey
	pl, err := a.state.GetPowerLevels(ctx, id.RoomID(child))
	if err != nil {
		return err
	}
	if len(evt.Content.AsSpaceChild().Via) == 0 || pl != nil && pl.GetUserLevel(evt.Sender) < pl.GetEventLevel(event.StateSpaceParent) {
		_, err := c.st.DB().ExecContext(ctx, `DELETE FROM matrix_space_children WHERE space_id = ?1 AND child_id = ?2`, space, child)
		return err
	}
	_, err = c.st.DB().ExecContext(ctx, `INSERT OR IGNORE INTO matrix_space_children (space_id, child_id) VALUES (?1, ?2)`, space, child)
	return err
}

// nest makes each stored room but a DM the child of a stored space listing
// it, the first by ID if several do, so that hiding a space hides its rooms.
// A room that would sit deeper than maxDepth goes under the deepest ancestor
// that fits; one that would close a cycle has no parent.
func (c *mxConnector) nest(ctx context.Context) error {
	rows, err := c.st.DB().QueryContext(ctx, `
SELECT c.id, COALESCE(c.parent_id, ''), COALESCE((
    SELECT min(s.space_id) FROM matrix_space_children AS s
    JOIN containers AS p ON p.id = s.space_id AND p.kind = 'space'
    WHERE s.child_id = c.id AND s.space_id != c.id AND c.kind != 'dm'), '')
FROM containers AS c ORDER BY c.id`)
	if err != nil {
		return err
	}
	var ids []string
	cur, want := map[string]string{}, map[string]string{}
	for rows.Next() {
		var id, p, w string
		if err := rows.Scan(&id, &p, &w); err != nil {
			rows.Close()
			return err
		}
		ids, cur[id], want[id] = append(ids, id), p, w
	}
	if err := rows.Close(); err != nil {
		return err
	}
	parents := nesting(ids, want)
	tx, err := c.st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if parents[id] == cur[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE containers SET parent_id = NULLIF(?2, '') WHERE id = ?1`, id, parents[id]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// nesting keeps the wanted parents that leave every chain acyclic, and
// moves a room that would sit deeper than maxDepth up to the deepest
// ancestor that fits.
func nesting(ids []string, want map[string]string) map[string]string {
	const visiting = 0
	depth := map[string]int{}
	parent := map[string]string{}
	var depthOf func(string) int
	depthOf = func(id string) int {
		if d, ok := depth[id]; ok {
			return d
		}
		depth[id] = visiting
		d := 1
		if p := want[id]; p != "" {
			pd := depthOf(p)
			if pd == maxDepth {
				p, pd = parent[p], pd-1
			}
			if pd != visiting {
				parent[id], d = p, pd+1
			}
		}
		depth[id] = d
		return d
	}
	for _, id := range slices.Sorted(slices.Values(ids)) {
		depthOf(id)
	}
	return parent
}
