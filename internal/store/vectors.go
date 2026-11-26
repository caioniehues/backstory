package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
)

// EmbeddedKinds is the one list of the Record kinds that get a vector:
// Decisions, Outcomes, Handoffs and Notes. Claims and the reserved kinds are
// never embedded.
func EmbeddedKinds() []RecordKind {
	return []RecordKind{KindDecision, KindOutcome, KindHandoff, KindNote}
}

// VectorHit is one Record a vector scan found, with the cosine similarity of
// its vector to the query vector.
type VectorHit struct {
	RecordID   string
	Similarity float64
}

// VectorScan is NearestVectors' result.
type VectorScan struct {
	// Hits are the closest Records, most similar first.
	Hits []VectorHit
	// Unembedded counts the Records under the filter of an EmbeddedKinds
	// kind that have no vector for the query's fingerprint and dimension
	// yet (not embedded, or embedded by another model). Non-zero means the
	// scan could not see every Record it should have.
	Unembedded int
}

// NearestVectors is the filtered brute-force nearest-vector scan: it reads
// every vector stored under model with len(query) dimensions for the
// Records under f (Tombstoned Records always excluded), and returns the
// f.Limit most cosine-similar to query, ties newest first. The filter is
// applied before ranking, so a narrow filter never comes back short. Vectors
// of another model or dimension are never compared. Nothing is cached: every
// call reads SQLite, so forgetting a Record needs no cache eviction.
func (s *Store) NearestVectors(model string, query []float32, f SearchFilter) (VectorScan, error) {
	if f.Limit <= 0 || len(query) == 0 {
		return VectorScan{}, nil
	}
	where, args, ok := s.searchWhere(f)
	if !ok {
		return VectorScan{}, nil
	}
	qnorm := norm(query)
	if qnorm == 0 {
		return VectorScan{}, nil
	}

	scanArgs := append([]any{model, len(query)}, args...)
	//nolint:gosec // where is assembled from fixed fragments and "?" placeholders; every value is bound
	rows, err := s.db.Query(`SELECT r.id, r.rowid, e.vec FROM record_embeddings e
		JOIN records r ON r.id = e.record_id
		WHERE e.model = ? AND e.dim = ? AND `+where, scanArgs...)
	if err != nil {
		return VectorScan{}, fmt.Errorf("store: nearest vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scored struct {
		hit   VectorHit
		rowid int64
	}
	var all []scored
	for rows.Next() {
		var (
			id    string
			rowid int64
			blob  []byte
		)
		if err := rows.Scan(&id, &rowid, &blob); err != nil {
			return VectorScan{}, fmt.Errorf("store: nearest vectors: %w", err)
		}
		vec, err := decodeVector(blob)
		if err != nil || len(vec) != len(query) {
			continue
		}
		vnorm := norm(vec)
		if vnorm == 0 {
			continue
		}
		var dot float64
		for i := range query {
			dot += float64(vec[i]) * float64(query[i])
		}
		all = append(all, scored{VectorHit{id, dot / (vnorm * qnorm)}, rowid})
	}
	if err := rows.Err(); err != nil {
		return VectorScan{}, fmt.Errorf("store: nearest vectors: %w", err)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].hit.Similarity != all[j].hit.Similarity {
			return all[i].hit.Similarity > all[j].hit.Similarity
		}
		return all[i].rowid > all[j].rowid
	})
	scan := VectorScan{}
	for i := 0; i < len(all) && i < f.Limit; i++ {
		scan.Hits = append(scan.Hits, all[i].hit)
	}

	kinds := make([]string, 0, 4)
	for _, k := range EmbeddedKinds() {
		kinds = append(kinds, string(k))
	}
	countArgs := append(append([]any{}, args...), jsonArray(kinds), model, len(query))
	//nolint:gosec // where is assembled from fixed fragments and "?" placeholders; every value is bound
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records r
		WHERE `+where+` AND r.kind IN (SELECT value FROM json_each(?))
		AND NOT EXISTS (SELECT 1 FROM record_embeddings e WHERE e.record_id = r.id AND e.model = ? AND e.dim = ?)`,
		countArgs...).Scan(&scan.Unembedded); err != nil {
		return VectorScan{}, fmt.Errorf("store: nearest vectors: count unembedded: %w", err)
	}
	return scan, nil
}

// RecordVector returns the vector stored for recordID under model, ok false
// when the Record has none for that fingerprint (or is unknown or
// Tombstoned). A Record query uses it, so nothing is re-embedded.
func (s *Store) RecordVector(recordID, model string) (vec []float32, ok bool, err error) {
	var blob []byte
	err = s.db.QueryRow(`SELECT e.vec FROM record_embeddings e JOIN records r ON r.id = e.record_id
		WHERE e.record_id = ? AND e.model = ? AND r.tombstoned_at IS NULL`, recordID, model).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: vector of %s: %w", recordID, err)
	}
	vec, err = decodeVector(blob)
	if err != nil {
		return nil, false, fmt.Errorf("store: vector of %s: %w", recordID, err)
	}
	return vec, true, nil
}

func norm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}
