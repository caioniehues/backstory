# How vectors fit in the single SQLite file under the pure-Go modernc driver

Research for issue #17 (child of the wayfinder map #14). Researched 2026-11-25 against `modernc.org/sqlite` **v1.59.0** (the version pinned in `go.mod`), its pinned `modernc.org/libc` **v1.75.7**, and the bundled sqlite-vec **v0.1.9**.

## Answer in brief

- **modernc cannot load native extensions, and it doesn't need to.** `load_extension()` can never succeed on Linux: the transpiled SQLite calls `libc.Xdlopen`, and in `modernc.org/libc` v1.75.7 that function is a stub that returns 0 (NULL). The driver also exposes no way to turn on `SQLITE_LoadExtension`. But **modernc ships sqlite-vec itself**: `modernc.org/sqlite/vec` is sqlite-vec v0.1.9 transpiled to Go. A blank import registers it on every connection through `sqlite3_auto_extension`. So `vec0` and the `vec_*` functions are available with no cgo and no shared library.
- **At Record scale, brute force is enough.** On this machine (Ryzen 5 7600X) an exact top-10 search over 10k × 768-d vectors takes about 4 ms in Go when the vectors are already in memory, about 32 ms when they must first be loaded from SQLite, and about 14 ms with `vec0`. At 100k × 768 (10× a large Record count) those figures are about 42 ms, about 300 ms and about 128 ms. Every method scales linearly.
- **Recommendation:** store one embedding per Record in a **plain table** (`record_id` → model, dim, float32 BLOB) and search it by brute force in Go, behind the planned Search seam's vector adapter. Keep `vec0` (already in the dependency tree) as the drop-in for when cold CLI latency or memory starts to matter. **Don't** add an ANN index: no stable, persisted-in-SQLite option exists today, and the counts don't need one. **Don't** embed raw Timeline events. At the million-event scale brute force no longer fits an interactive budget, and those events are mostly tool noise.
- **Caveat on sizing:** **this machine has no Backstory store.** The binary is not installed and `$XDG_DATA_HOME/backstory/backstory.db` does not exist, so there are no real Record or Timeline event counts to report (counts: 0 / 0, no store). The latency figures below are measured over a grid of N, so they still hold. Rerun the two `COUNT(*)` queries in "Sizing" once a store exists to see where it falls.

Confidence: **high** on the driver facts (read from the module source) and on the measurements (taken on this machine; the cost of a brute-force search doesn't depend on the data). **Medium** on the scale assumptions, because no store exists here.

## 1. Can `modernc.org/sqlite` load extensions such as sqlite-vec?

**Loading a native `.so` is impossible.** In the module source:

- The transpiled SQLite does contain `sqlite3_load_extension` (`lib/sqlite_g_0000000000003a00.go`, `_sqlite3LoadExtension`). It honours the usual guard: "ability to run load_extension is turned off by default. One must call either sqlite3_enable_load_extension(db) or sqlite3_db_config(db, SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION, 1, 0)".
- `unixDlOpen` calls `libc.Xdlopen` (`lib/sqlite_g_000000000001ffff.go:12651`). In `modernc.org/libc@v1.75.7/libc_musl.go:834`, which is the build for `linux && (amd64 || arm64 || …)`, `Xdlopen` has the body `return 0`. On darwin, the BSDs and Windows it is `panic(todo(""))`. A load attempt therefore always fails at dlopen.
- The hand-written driver files (`*.go` at the module root) never mention `load_extension` or `enable_load_extension`. No API exists to turn it on.
- Even if dlopen worked, a native extension would be C code using the system allocator and libc. The transpiled SQLite lives in Go-managed memory through `modernc.org/libc`, so the two cannot share `sqlite3*` handles.

**sqlite-vec is bundled in transpiled form.** Since v1.47.0 (2026-03-17, CHANGELOG: "Add CGO-free version of the vector extensions from https://github.com/asg017/sqlite-vec"), the module carries a `vec/` package. Its package doc (`vec/patches.go`) reads:

> Package vec bundles sqlite-vec … in the same CGo-free form as the rest of this module. The bundled version is v0.1.9 … The package exports no API. Import it for its side effect, which installs the extension with sqlite3_auto_extension so that every connection opened afterwards has it loaded.

```go
import (
	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)
```

I confirmed this works by running it: `CREATE VIRTUAL TABLE … USING vec0(embedding float[768] distance_metric=cosine)`, inserts, `WHERE embedding MATCH ? AND k = 10` KNN queries and `vec_distance_cosine()` all ran under v1.59.0 with no cgo. Facts that matter for a build decision:

- **Version coupling.** v1.59.0's CHANGELOG says "`vec/` stays at sqlite-vec v0.1.9". The vec version moves only when modernc re-vendors it, and modernc pins `modernc.org/libc` exactly (the `go.mod` already pins v1.75.7 for this reason).
- **Pre-v1 extension.** The v1.47.0 CHANGELOG entry quotes upstream: "sqlite-vec is a pre-v1, so expect breaking changes!"
- **License.** sqlite-vec is MIT (Alex Garcia). The module ships `LICENSE-SQLITE_VEC`, and the MIT notice must travel with redistributed builds (relevant to the AUR package).
- **Binary size.** A minimal program grows from 9.68 MB to 10.43 MB, about **+0.75 MB**, when `vec` is imported.
- **Other tools see the store differently.** The stock `sqlite3` CLI (3.54.1 here) opens a store containing a `vec0` table. `.tables`, `PRAGMA integrity_check`, `quick_check` and `VACUUM INTO` all succeed. **Querying the vec0 table fails with `no such module: vec0`.** Inside Backstory this is harmless as long as the blank import lives in `internal/store`, so every binary that opens the store has the module. A migration that runs `CREATE VIRTUAL TABLE … USING vec0` in a binary without the import would fail.

The modernc driver also has a pure-Go virtual-table API (`modernc.org/sqlite/vtab`, since v1.45.0; the README even sketches a "Vector search (sqlite-vec style)" module). That makes a home-grown Go index usable from SQL possible, but nothing calls for it.

## 2. Ways to keep vectors in the one SQLite file

| Option | Where the vectors live | Query | For | Against |
|---|---|---|---|---|
| **A. Plain BLOB table + Go brute force** (recommended) | Ordinary table, one little-endian float32 BLOB per Record | Load all rows, compute dot products (vectors pre-normalised, so dot product equals cosine), keep a top-k heap | No new dependency surface. Any tool can read the table. Forgetting is an ordinary `DELETE` under the store's existing `secure_delete(ON)`. Exact results. Easy to test. | Each query in a short-lived CLI process re-reads every vector (cold ≈ 32 ms at 10k×768, ≈ 300 ms at 100k×768). Resident memory is N×D×4 bytes while the slab is held. A daemon-held cache brings it down to ≈ 4 / 42 ms. |
| **B. `vec0` virtual table** (`modernc.org/sqlite/vec`) | Shadow tables `<t>_chunks`, `<t>_rowids`, `<t>_vector_chunksNN`, `<t>_info`. Vectors are packed into chunk BLOBs. | `WHERE embedding MATCH ? AND k = ?`, an exact chunked scan inside SQLite | Faster than A when cold (≈ 14 ms at 10k×768, ≈ 128 ms at 100k×768) with no Go-heap slab. Partition keys (e.g. `project_key`) shard the scan. Supports int8 and bit vectors. | Pre-v1 extension whose upgrades come only with modernc releases. The stock sqlite3 CLI can't query it. Adds the MIT notice and +0.75 MB. |
| **C. Plain BLOB table + `vec_distance_cosine()` in SQL** | Same as A | `ORDER BY vec_distance_cosine(v, ?) LIMIT k` | Plain table, so no module is needed to read the schema | Slower than both A-cold and B (22.5 ms at 10k×768, 204 ms at 100k×768). Still needs the vec import for the function. No reason to choose it. |
| **D. Pure-Go HNSW graph persisted as a BLOB** (e.g. `github.com/coder/hnsw`) | Vectors in A's table, plus a serialised graph in one BLOB row | Approximate search over an in-memory graph | Sub-linear query time at millions of vectors | The README says "all graph operations are in-memory". Persistence is `Graph.Export`/`Import` to an `io.Writer`/`io.Reader` (or a `SavedGraph` file, which would break the single-file rule). Every Delete must re-export the whole graph, or the forgotten vector lingers in the serialised copy. The graph has to be rebuilt or loaded per CLI process. No releases; installed from `@main`. |
| **E. sqlite-vec's own ANN indexes** (DiskANN, rescore, IVF) | vec0 shadow tables | ANN inside SQLite | Would be the natural upgrade from B | Available **only in the v0.1.10 alpha pre-releases** (alpha.1, 31 March: "new ANN indexes: rescore, ivf (experimental, not enabled), and DiskANN"; alpha.2 fixed "data leaking via un-deleted compressed neighbor vectors in DiskANN"). modernc bundles v0.1.9, so none of it is reachable from this driver today. |
| **F. Go `vtab` module of our own** | Our choice | SQL through `modernc.org/sqlite/vtab` | Full control | A custom index to build and maintain. Only worth it if A and B both fail. |

A sidecar index file, a vector database or a localhost service would each break the one-file or no-network constraint, so they are out of scope.

**Why A over B as the starting point:** Records are few, and A keeps the forgetting story and the schema identical to everything else in the store. B's advantage is cold CLI latency and memory, and it only becomes material above about 50k vectors. If the vector adapter sits behind the Search seam, switching to B later is a migration plus an adapter change, not a redesign. If the decision ticket prefers B from day one, the facts above support that too. The main costs are the pre-v1 format and the CLI blind spot.

## 3. Sizing

### Real counts on this machine

No store exists (`backstory` is not on `PATH`; `$XDG_DATA_HOME/backstory/backstory.db` is absent). **Records: 0. Timeline events: 0.** With a store, these are the only queries the ticket allows (counts only):

```sh
sqlite3 -readonly "$XDG_DATA_HOME/backstory/backstory.db" \
  "SELECT COUNT(*) FROM records; SELECT COUNT(*) FROM records WHERE tombstoned_at IS NULL; SELECT COUNT(*) FROM timeline_events;"
```

Without real counts, I assume Records number in the low thousands to about 10k for one heavy user, with 10× = 100k. Timeline events run 100–1000× the Records (every tool use and result is an event), so roughly 1M.

### Measured (synthetic, AMD Ryzen 5 7600X, Go 1.27.4, modernc v1.59.0, WAL, `secure_delete(ON)`, median of 5, top-10)

| N × D | Disk per copy* | Load all from SQLite (CLI cold) | Go scan, warm | Go heap for the slab | `vec0` KNN | SQL `vec_distance_cosine` |
|---|---|---|---|---|---|---|
| 1k × 768 | ≈ 3.7 MB | 3.1 ms | 0.46 ms | 5 MB | 1.5 ms | – |
| 10k × 384 | ≈ 18 MB | 16.9 ms | 2.0 ms | 18 MB | 6.8 ms | – |
| 10k × 768 | ≈ 36 MB | 27.9 ms | 4.3 ms | 33 MB | 13.9 ms | 22.5 ms |
| 10k × 1024 | ≈ 44 MB | 31.8 ms | 5.7 ms | 44 MB | 17.5 ms | – |
| 100k × 384 | ≈ 181 MB | 152 ms | 20 ms | 187 MB | 66 ms | – |
| 100k × 768 | ≈ 361 MB | 262 ms | 42 ms | 322 MB | 128 ms | 204 ms |
| 100k × 1024 | ≈ 437 MB | 320 ms | 57 ms | 462 MB | 168 ms | – |

\* The benchmark file held both a plain table and a vec0 copy. The figure is half the file size, which is close to N × D × 4 bytes. Inserting both copies of 100k × 768 took 12.4 s in one transaction (a one-off backfill cost; the GPU embedding step will dominate it).

The Go scan is a naive single-threaded loop. Unrolling, SIMD or goroutines would cut it further, but it is already small next to the load. Measured setup: `internal/store`-style DSN (WAL, `secure_delete(ON)`), random unit vectors, table `emb(rid INTEGER PRIMARY KEY, v BLOB)` and `vec0(embedding float[D] distance_metric=cosine)`. The throwaway benchmark was not committed.

### What it means at Backstory's counts

- **Records (≈ 10k now, 100k at 10×)**: every option fits. At 10k × 768: A costs ≈ 32 ms cold or ≈ 4 ms with a daemon cache, ≈ 33 MB transient; B costs ≈ 14 ms with no slab. At 100k × 768: A costs ≈ 0.3 s cold (noticeable for a CLI `recall`, fine for a daemon-cached path), B ≈ 0.13 s. Memory for A at 10× is about 320 MB while held. Pick a smaller model (384-d halves everything) or quantise if that matters (vec0 `int8[D]` = ¼, `bit[D]` = 1/32).
- **Timeline events (≈ 1M; 10M at 10×)**: extrapolating linearly, A at 1M × 768 is ≈ 2.6 s cold / 0.4 s warm with ≈ 3 GB resident. B is ≈ 1.3 s with a ≈ 3.6 GB file. Neither suits interactive use, and the file would grow several-fold (the roadmap's Purge and backup set would carry it too). Embedding raw events would need ANN (unavailable, see E) or heavy quantisation plus partitioning. **So don't embed raw Timeline events.** Embed Records, and if needed one derived summary per Session.
- The query embedding itself (one GPU call through the CLI or unix socket, ADR 0002) is a separate cost, covered by the runtime ticket.

## 4. What `SCHEMA.md` and the migration code require of a new table

From `SCHEMA.md` and `internal/store/migrate.go`:

1. **A new numbered migration file**, `internal/store/migrations/NNNN_<name>.sql`. The next free number at this HEAD is **0028**. `SchemaVersion` is derived from the highest file, so there is no constant to bump. Two files sharing a number are refused at load ("a duplicate version is skipped as already applied and never runs"), so parallel branches must renumber before merging.
2. **It runs in one transaction**, on a dedicated connection with `PRAGMA foreign_keys = OFF`, and `PRAGMA foreign_key_check` runs before commit. Any `REFERENCES records(id)` must therefore hold for every row the migration writes. The migration may race a concurrent Open (it re-checks `schema_version` under the write lock), so the DDL must be plain and deterministic.
3. **No embedding work inside the migration.** `migrationDataHooks` run inside the migration transaction. Calling the GPU CLI there would hold the write lock for minutes and make Open depend on the GPU. Create the table empty and backfill embeddings out of band, idempotently.
4. **The table is derived data, not Ledger.** Invariant 1 ("No UPDATE, no DELETE on `records` or `timeline_events`") does not apply to it, and it **must** allow DELETE. Don't copy the `*_no_delete` triggers. The precedent is `records_fts`: `TombstoneRecord` (`internal/store/records.go:476`) removes the FTS text in the same transaction as the tombstone and turns on FTS5 `secure-delete`. The vector row must be deleted in that same transaction. A project `purge` tombstones Records through the same path. If events are ever embedded, `PurgeSessions` must delete their vectors too. "Forgetting must reach every stored copy" includes the vector: an embedding is derived from the text and can partly reveal it. With option B, sqlite-vec v0.1.9's delete path zeroes the vector bytes in place (`vec0Update_Delete_ClearVectors` writes a zero buffer with `sqlite3_blob_write`), clears the validity and rowid entries, and drops empty chunks. With option A it is an ordinary row delete, which `secure_delete(ON)` zeroes. Backups made with `VACUUM INTO` carry vectors exactly as they carry Record text, so the forgetting module (audit #5) covers them with no change.
5. **Each invariant needs a mutation-probe test** ("Each carries a mutation probe in the first punch: allow the forbidden write → RED; unmutated → GREEN"). For this table: Delete of a Record leaves no vector row, or with vec0 no non-zero bytes. A tombstoned Record never appears in vector results.
6. **No sync.** `sync_changes.kind` is limited to `record`, `tombstone`, `edge` and `session`, and its triggers fire only on those tables. Vectors are per-Device derived data (each Device embeds its own Records, including Records imported from a Peer), so add no sync trigger and no change kind.
7. **Format changes are schema changes.** SCHEMA.md: "a payload shape change is a schema change". Store `model` and `dim` on every vector row, so that a model swap is a re-embed (rows with a stale model are ignored, then replaced) rather than a silent mix of incompatible vectors.
8. **Update the DDL sketch in `SCHEMA.md`** next to `records_fts`. With option B, the `modernc.org/sqlite/vec` blank import goes in `internal/store/store.go` beside the driver import, so every binary that migrates or opens the store has the module.

## Sources

- `modernc.org/sqlite@v1.59.0` module source (downloaded with `go mod download`; origin `https://gitlab.com/cznic/sqlite` tag `v1.59.0`): `vec/patches.go` (package doc, auto-extension), `vec_test.go`, `CHANGELOG.md` (v1.45.0 vtab, v1.47.0 vec, v1.50.0 sqlite-vec v0.1.9, v1.57.0 LICENSE-SQLITE_VEC, v1.59.0 "vec/ stays at sqlite-vec v0.1.9"), `README.md` (vtab section), `lib/sqlite_g_0000000000003a00.go` (`_sqlite3LoadExtension`), `lib/sqlite_g_000000000001ffff.go` (`_unixDlOpen`), `vec/vec_g_0000000000000154.go` (`Xvec0Update_Delete_ClearVectors`).
- `modernc.org/libc@v1.75.7`: `libc_musl.go:834` (`Xdlopen` returns 0 on Linux) and the darwin, BSD and Windows stubs.
- sqlite-vec docs via Context7 (`/websites/alexgarcia_xyz_sqlite-vec`, `/asg017/sqlite-vec`): KNN with vec0 or scalar functions, metadata and partition-key columns, shadow-table layout (`ARCHITECTURE.md`), quantisation.
- sqlite-vec releases, https://github.com/asg017/sqlite-vec/releases: v0.1.9 is latest stable; ANN only in v0.1.10-alpha.1 to alpha.4.
- coder/hnsw README, https://github.com/coder/hnsw: in-memory graph, Export/Import and SavedGraph persistence, Delete supported.
- This repo: `SCHEMA.md` (invariants, records_fts, sync change kinds, payload-shape rule), `internal/store/migrate.go`, `internal/store/store.go` (DSN pragmas), `internal/store/records.go` (`TombstoneRecord`), `internal/store/backup.go` (`VACUUM INTO` + `integrity_check`), `cmd/backstory/daemon.go` (store path).
- Measurements: throwaway Go benchmark run on this machine, 2026-11-25 (not committed).
