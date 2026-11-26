# Record vectors are derived, Device-local data that every forgetting path deletes

Each Record's embedding lives in its own table, `record_embeddings`, with one row per Record keyed by `record_id`. The vector is derived data: it can be recomputed from the Record at any time. Each Device computes its own vectors with its own runtime. They never sync and are never exported, and a Device without a GPU stays FTS-only unless the Human opts in to embedding on the CPU with `backstory embed --on --allow-cpu`; the backend (`cpu`) is then part of the fingerprint, so CPU vectors are never compared with GPU ones. The table is outside the Ledger's append-only rule: rows are upserted when the model changes and deleted when their Record is forgotten.

Forgetting reaches vectors in one place. The vector `DELETE` lives in `tombstoneRecordTx`, which Delete, Purge and tombstones arriving by Sync all go through, so it runs in the same transaction as the `records_fts` scrub. The write side can't bring back a forgotten Record either: a vector is inserted only by a single statement that checks the Record isn't Tombstoned. Record text reaches the embedding runtime only over stdin, never through a temp file, so the table is the only stored copy.

The decision and its reasons are in [#20](https://github.com/caioniehues/backstory/issues/20).

## Considered Options

- **Sync vectors alongside Records (rejected).** A Device without a GPU would get semantic search for free. But a vector is a copy of the Record's meaning. Syncing it would add one more thing that every Peer's forgetting must reach, and the vector would only be usable if both Devices ran the same model on the same backend (#19 found Vulkan and HIP give slightly different vectors).
- **Store vectors in the append-only Ledger (rejected).** Then a model change could only add rows, never replace them, and Delete would need a second exception to the "no DELETE" rule. Keeping vectors as plainly derived, mutable data keeps the Ledger's rules unchanged.
- **Delete vectors in each forgetting caller (rejected).** There are three callers: `TombstoneRecord`, `PurgeSessions` and Sync import. A fourth would be easy to add and forget. One hook in the shared transaction body can't be bypassed.
