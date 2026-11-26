package store

import (
	"fmt"
	"strings"
	"time"
)

// Confirmation is one live confirm Record that confirms a target: Done,
// Affirm or Promote, the three actions that mint an `informs` edge
// (confirmEdgeType). Contradict and Supersede are not confirmations; their
// edges are real `contradicts` and `supersedes` edges.
type Confirmation struct {
	// ID is the confirm Record's own id.
	ID string
	// Type is ConfirmDone, ConfirmAffirm or ConfirmPromote.
	Type ConfirmAction
	// Tier is the confirm Record's own Tier: who confirmed.
	Tier Tier
	TS   time.Time
}

// ConfirmationsOf returns, for each of ids that has any, its live
// confirmations, oldest first by sequence. An id with none has no entry.
//
// Type comes from the columns Confirm wrote, never from the target's text:
// a promote always stamps its promoter, a Done is the human-declared
// DoneMarkerText with no promoter (HandoffDone's rule), and any other
// informing confirm is an affirm. A Tombstoned confirm confirms nothing.
func (s *Store) ConfirmationsOf(ids []string) (map[string][]Confirmation, error) {
	out := map[string][]Confirmation{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ids))
	args := []any{string(ConfirmPromote), string(TierHumanDeclared), DoneMarkerText, string(ConfirmDone), string(ConfirmAffirm),
		string(EdgeInforms), string(KindConfirm)}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	//nolint:gosec // the concatenated part is only "?" placeholders (one per id), every value is still bound as a query arg
	query := `SELECT e.to_id, c.id, CASE
			WHEN c.promoter IS NOT NULL THEN ?
			WHEN c.tier = ? AND c.text = ? THEN ?
			ELSE ? END, c.tier, c.ts
		FROM edges e JOIN records c ON c.id = e.from_id
		WHERE e.type = ? AND c.kind = ? AND c.tombstoned_at IS NULL
		AND e.to_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY c.rowid ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: confirmations of %d records: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target, typ, tier string
		var c Confirmation
		var nanos int64
		if err := rows.Scan(&target, &c.ID, &typ, &tier, &nanos); err != nil {
			return nil, fmt.Errorf("store: confirmations: %w", err)
		}
		c.Type, c.Tier, c.TS = ConfirmAction(typ), Tier(tier), tsFromNanos(nanos)
		out[target] = append(out[target], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: confirmations: %w", err)
	}
	return out, nil
}
