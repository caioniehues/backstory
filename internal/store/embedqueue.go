package store

import (
	"fmt"
)

// QueuedRecord is one Record waiting for a vector: its id and the document
// the drain embeds for it.
type QueuedRecord struct {
	RecordID string
	// Document is the Record's text, plus a blank line and next for a
	// Handoff with one.
	Document string
}

// EmbedCounts is how far the Records of the EmbeddedKinds are embedded
// under one fingerprint.
type EmbedCounts struct {
	// Embedded counts the Records with a vector for the fingerprint.
	Embedded int
	// Queued counts the Records still without one (EmbedQueue's length).
	Queued int
}

// embedQueueWhere is the queue: a Record of an EmbeddedKinds kind, not
// Tombstoned, with no vector for the fingerprint bound to its second
// placeholder (the first binds the kinds). Superseded Records, Drafts and
// Records from Peers are all in it.
const embedQueueWhere = `r.kind IN (SELECT value FROM json_each(?)) AND r.tombstoned_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM record_embeddings e WHERE e.record_id = r.id AND e.model = ?)`

// embeddedKindsJSON is EmbeddedKinds as a JSON array for json_each.
func embeddedKindsJSON() string {
	kinds := make([]string, 0, 4)
	for _, k := range EmbeddedKinds() {
		kinds = append(kinds, string(k))
	}
	return jsonArray(kinds)
}

// EmbedQueue returns up to limit Records that have no vector for model (the
// embedding fingerprint) yet, newest first, leaving out the ids in exclude
// (the drain's skipped Records). A model change puts every Record back in
// the queue; the upsert then replaces each old vector in place.
func (s *Store) EmbedQueue(model string, limit int, exclude []string) ([]QueuedRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	if exclude == nil {
		exclude = []string{}
	}
	rows, err := s.db.Query(`SELECT r.id, r.text, r.kind, COALESCE(r.next, '') FROM records r
		WHERE `+embedQueueWhere+` AND r.id NOT IN (SELECT value FROM json_each(?))
		ORDER BY r.rowid DESC LIMIT ?`,
		embeddedKindsJSON(), model, jsonArray(exclude), limit)
	if err != nil {
		return nil, fmt.Errorf("store: embed queue: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []QueuedRecord
	for rows.Next() {
		var id, text, kind, next string
		if err := rows.Scan(&id, &text, &kind, &next); err != nil {
			return nil, fmt.Errorf("store: embed queue: %w", err)
		}
		doc := text
		if RecordKind(kind) == KindHandoff && next != "" {
			doc += "\n\n" + next
		}
		out = append(out, QueuedRecord{RecordID: id, Document: doc})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: embed queue: %w", err)
	}
	return out, nil
}

// EmbedCounts counts the Records of the EmbeddedKinds (not Tombstoned) with
// and without a vector for model.
func (s *Store) EmbedCounts(model string) (EmbedCounts, error) {
	var c EmbedCounts
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records r WHERE `+embedQueueWhere,
		embeddedKindsJSON(), model).Scan(&c.Queued); err != nil {
		return EmbedCounts{}, fmt.Errorf("store: embed counts: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records r JOIN record_embeddings e ON e.record_id = r.id
		WHERE r.kind IN (SELECT value FROM json_each(?)) AND r.tombstoned_at IS NULL AND e.model = ?`,
		embeddedKindsJSON(), model).Scan(&c.Embedded); err != nil {
		return EmbedCounts{}, fmt.Errorf("store: embed counts: %w", err)
	}
	return c, nil
}
