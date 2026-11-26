-- 0028_record_embeddings.sql — one vector per Record (#39, ADR-0003).
-- Derived, Device-local, mutable data outside the append-only invariants:
-- no sync trigger, no no-delete triggers. Rows are written only by the
-- conditional upsert (UpsertRecordEmbedding) and deleted only by
-- tombstoneRecordTx. This migration does no embedding work.
CREATE TABLE record_embeddings (
  record_id  TEXT PRIMARY KEY REFERENCES records(id),
  model      TEXT NOT NULL,     -- fingerprint: model file sha256 + prompt-template version + backend + dim
  dim        INTEGER NOT NULL,
  vec        BLOB NOT NULL      -- float32 × dim
);
