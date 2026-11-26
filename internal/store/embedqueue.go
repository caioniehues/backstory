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

// embeddedKind is the condition over records r that r is of an
// EmbeddedKinds kind. It binds embeddedKindsJSON().
const embeddedKind = "r.kind IN (SELECT value FROM json_each(?))"

// unembedded is the condition over records r that r is of an EmbeddedKinds
// kind with no vector for fingerprint (of dim dimensions, when dim > 0),
// with its args in placeholder order. It is the one definition of "waiting
// for a vector" the queue, its counts and the vector scan's Unembedded
// share. It says nothing about Tombstones: each caller excludes them.
func unembedded(fingerprint string, dim int) (cond string, args []any) {
	cond = embeddedKind + " AND NOT EXISTS (SELECT 1 FROM record_embeddings e WHERE e.record_id = r.id AND e.model = ?"
	args = []any{embeddedKindsJSON(), fingerprint}
	if dim > 0 {
		cond += " AND e.dim = ?"
		args = append(args, dim)
	}
	return cond + ")", args
}

// embedQueue is the queue as a condition over records r, with its args: a
// Record of an EmbeddedKinds kind, not Tombstoned, with no vector for
// fingerprint. Superseded Records, Drafts and Records from Peers are all in
// it.
func embedQueue(fingerprint string) (cond string, args []any) {
	cond, args = unembedded(fingerprint, 0)
	return "r.tombstoned_at IS NULL AND " + cond, args
}

// embeddedKindsJSON is EmbeddedKinds as a JSON array for json_each.
func embeddedKindsJSON() string {
	kinds := make([]string, 0, 4)
	for _, k := range EmbeddedKinds() {
		kinds = append(kinds, string(k))
	}
	return jsonArray(kinds)
}

// EmbedQueue returns up to limit Records that have no vector for
// fingerprint yet, newest first, leaving out the ids in exclude
// (the drain's skipped Records). A model change puts every Record back in
// the queue; the upsert then replaces each old vector in place.
func (s *Store) EmbedQueue(fingerprint string, limit int, exclude []string) ([]QueuedRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	if exclude == nil {
		exclude = []string{}
	}
	queue, args := embedQueue(fingerprint)
	//nolint:gosec // queue is assembled from fixed fragments and "?" placeholders; every value is bound
	rows, err := s.db.Query(`SELECT r.id, r.text, r.kind, COALESCE(r.next, '') FROM records r
		WHERE `+queue+` AND r.id NOT IN (SELECT value FROM json_each(?))
		ORDER BY r.rowid DESC LIMIT ?`,
		append(args, jsonArray(exclude), limit)...)
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
// and without a vector for fingerprint.
func (s *Store) EmbedCounts(fingerprint string) (EmbedCounts, error) {
	var c EmbedCounts
	queue, args := embedQueue(fingerprint)
	//nolint:gosec // queue is assembled from fixed fragments and "?" placeholders; every value is bound
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records r WHERE `+queue, args...).Scan(&c.Queued); err != nil {
		return EmbedCounts{}, fmt.Errorf("store: embed counts: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records r JOIN record_embeddings e ON e.record_id = r.id
		WHERE `+embeddedKind+` AND r.tombstoned_at IS NULL AND e.model = ?`,
		embeddedKindsJSON(), fingerprint).Scan(&c.Embedded); err != nil {
		return EmbedCounts{}, fmt.Errorf("store: embed counts: %w", err)
	}
	return c, nil
}
