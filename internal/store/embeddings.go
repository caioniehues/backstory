package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// UpsertRecordEmbedding stores vec as recordID's vector under model (the
// embedding fingerprint), replacing any vector the Record already has. It is
// one conditional statement: nothing is written unless the Record exists and
// is not Tombstoned, so an embedding that finishes after its Record was
// forgotten can't bring it back (ADR-0003). written reports whether a row was
// stored. The vector is stored as little-endian float32 × len(vec).
func (s *Store) UpsertRecordEmbedding(recordID, model string, vec []float32) (written bool, err error) {
	if len(vec) == 0 {
		return false, fmt.Errorf("store: upsert vector for %s: empty vector", recordID)
	}
	res, err := s.db.Exec(`INSERT INTO record_embeddings (record_id, model, dim, vec)
		SELECT id, ?, ?, ? FROM records WHERE id = ? AND tombstoned_at IS NULL
		ON CONFLICT(record_id) DO UPDATE SET model = excluded.model, dim = excluded.dim, vec = excluded.vec`,
		model, len(vec), encodeVector(vec), recordID)
	if err != nil {
		return false, fmt.Errorf("store: upsert vector for %s: %w", recordID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: upsert vector for %s: %w", recordID, err)
	}
	return n > 0, nil
}

// DropRecordEmbeddings deletes every vector (`backstory embed --drop`) and
// checkpoints the WAL, as a Tombstone does: the DSN's secure_delete zeroes
// the freed pages, and the checkpoint folds the WAL's page images in and
// truncates it, so no copy of a Record's meaning is left on disk. It is the
// only vector DELETE outside tombstoneRecordTx, and it never takes a WHERE:
// one Record's vector is forgotten only with the Record. Records are
// untouched; while embeddings are on, the drain embeds them again.
func (s *Store) DropRecordEmbeddings() (int, error) {
	res, err := s.db.Exec(`DELETE FROM record_embeddings`)
	if err != nil {
		return 0, fmt.Errorf("store: drop vectors: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: drop vectors: %w", err)
	}
	if err := s.checkpointTruncate(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// encodeVector is vec as little-endian float32s.
func encodeVector(vec []float32) []byte {
	b := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// decodeVector is encodeVector's inverse.
func decodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("store: vector blob is not a whole number of float32s")
	}
	vec := make([]float32, len(b)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return vec, nil
}
