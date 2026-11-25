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
