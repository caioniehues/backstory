package store

import (
	"encoding/binary"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const testModel = "sha256:abc|tmpl1|vulkan|3"

// storedVector reads record id's vector row: model, dim and the decoded
// vector, ok false when there is no row.
func storedVector(t *testing.T, s *Store, id string) (model string, dim int, vec []float32, ok bool) {
	t.Helper()
	rows, err := s.db.Query(`SELECT model, dim, vec FROM record_embeddings WHERE record_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", 0, nil, false
	}
	var blob []byte
	if err := rows.Scan(&model, &dim, &blob); err != nil {
		t.Fatal(err)
	}
	vec, err = decodeVector(blob)
	if err != nil {
		t.Fatal(err)
	}
	return model, dim, vec, true
}

func mustUpsertVector(t *testing.T, s *Store, id, model string, vec []float32) {
	t.Helper()
	ok, err := s.UpsertRecordEmbedding(id, model, vec)
	if err != nil || !ok {
		t.Fatalf("UpsertRecordEmbedding(%s) = %v, %v; want written", id, ok, err)
	}
}

func TestUpsertRecordEmbeddingWritesAndReplacesInPlace(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	id := insertFiller(t, s, "a decision worth a vector")

	mustUpsertVector(t, s, id, testModel, []float32{0.25, -1, 3.5})
	model, dim, vec, ok := storedVector(t, s, id)
	if !ok || model != testModel || dim != 3 || !slices.Equal(vec, []float32{0.25, -1, 3.5}) {
		t.Fatalf("stored = %q %d %v %v", model, dim, vec, ok)
	}

	mustUpsertVector(t, s, id, "other-model|4", []float32{1, 2, 3, 4})
	model, dim, vec, _ = storedVector(t, s, id)
	if model != "other-model|4" || dim != 4 || !slices.Equal(vec, []float32{1, 2, 3, 4}) {
		t.Fatalf("after re-embed: %q %d %v, want the new vector", model, dim, vec)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM record_embeddings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want 1 (replaced in place)", n, err)
	}
}

// A late embedding for a forgotten Record writes nothing, so it can't bring
// the Record's meaning back.
func TestUpsertRecordEmbeddingRefusesTombstonedAndUnknown(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	id := insertFiller(t, s, "soon forgotten")
	if err := s.TombstoneRecord(id, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	for _, rid := range []string{id, "no-such-record"} {
		ok, err := s.UpsertRecordEmbedding(rid, testModel, []float32{1, 2, 3})
		if err != nil || ok {
			t.Errorf("UpsertRecordEmbedding(%s) = %v, %v; want not written, no error", rid, ok, err)
		}
		if _, _, _, ok := storedVector(t, s, rid); ok {
			t.Errorf("%s has a vector row", rid)
		}
	}
	if _, err := s.UpsertRecordEmbedding(insertFiller(t, s, "x"), testModel, nil); err == nil {
		t.Error("an empty vector was accepted")
	}
}

func TestDeleteRemovesTheVector(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	id := insertFiller(t, s, "deleted soon")
	kept := insertFiller(t, s, "kept")
	mustUpsertVector(t, s, id, testModel, []float32{1, 2, 3})
	mustUpsertVector(t, s, kept, testModel, []float32{4, 5, 6})
	if err := s.TombstoneRecord(id, Identity{Kind: IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := storedVector(t, s, id); ok {
		t.Error("the deleted Record's vector survived")
	}
	if _, _, _, ok := storedVector(t, s, kept); !ok {
		t.Error("another Record's vector was removed")
	}
}

func TestPurgeRemovesTheVector(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	mustUpsertProject(t, s, "proj-a")
	sid := mustStartSessionInProject(t, s, "proj-a")
	id, err := s.InsertRecord(InsertRecordParams{
		Identity: Identity{Kind: IdentityAgent}, Kind: KindDecision, Text: "purged soon",
		SessionID: sid, ProjectKey: "proj-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	mustUpsertVector(t, s, id, testModel, []float32{1, 2, 3})
	counts, err := s.PurgeSessions(PurgeScope{ProjectKey: "proj-a"}, Identity{Kind: IdentityHuman})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Records != 1 {
		t.Fatalf("purge tombstoned %d records, want 1", counts.Records)
	}
	if _, _, _, ok := storedVector(t, s, id); ok {
		t.Error("the purged Record's vector survived")
	}
}

func TestSyncedTombstoneRemovesTheVector(t *testing.T) {
	a := mustOpen(t, filepath.Join(t.TempDir(), "a.db"))
	b := mustOpen(t, filepath.Join(t.TempDir(), "b.db"))
	seedRemoteProject(t, a, "/a/widgets", "https://github.com/acme/widgets")
	seedRemoteProject(t, b, "/b/widgets", "git@github.com:acme/widgets.git")
	id := insertBranchHandoff(t, a, "/a/widgets", "main")
	human := Identity{Kind: IdentityHuman, Actor: humanActor}

	changes, cursor, _, err := a.ChangesSince(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ImportSyncChanges(human, SyncPeer{DeviceID: "dev-a"}, changes); err != nil {
		t.Fatal(err)
	}
	mustUpsertVector(t, b, id, testModel, []float32{1, 2, 3})

	if err := a.TombstoneRecord(id, human); err != nil {
		t.Fatal(err)
	}
	changes, _, _, err = a.ChangesSince(cursor, 1000)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.ImportSyncChanges(human, SyncPeer{DeviceID: "dev-a"}, changes)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tombstones != 1 {
		t.Fatalf("import = %+v, want one tombstone", res)
	}
	if _, _, _, ok := storedVector(t, b, id); ok {
		t.Error("the synced tombstone left B's vector behind")
	}
}

// A backup is the DB file, so it carries the vectors unchanged.
func TestBackupCarriesVectors(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	id := insertFiller(t, s, "backed up")
	mustUpsertVector(t, s, id, testModel, []float32{0.5, 0.25, -2})
	dest := filepath.Join(t.TempDir(), "snap.db")
	if _, err := s.Backup(dest, false); err != nil {
		t.Fatal(err)
	}
	r := mustOpen(t, dest)
	model, dim, vec, ok := storedVector(t, r, id)
	if !ok || model != testModel || dim != 3 || !slices.Equal(vec, []float32{0.5, 0.25, -2}) {
		t.Fatalf("backup vector = %q %d %v %v", model, dim, vec, ok)
	}
}

// The table is Device-local, mutable data: no trigger syncs it or guards it
// against deletes.
func TestRecordEmbeddingsHasNoTriggers(t *testing.T) {
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND sql LIKE '%record_embeddings%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d triggers mention record_embeddings, want 0", n)
	}
}

// Forgetting one Record's vector happens in one place, tombstoneRecordTx, so
// every forgetting path (Delete, Purge, synced tombstones) reaches it. The
// only other vector DELETE is `embed --drop`'s, which removes every row: no
// path deletes a single Record's vector except a Tombstone.
func TestOnlyTombstoneDeletesVectors(t *testing.T) {
	const stmt = "DELETE FROM record_embeddings"
	funcName := regexp.MustCompile(`^func (?:\([^)]*\) )?(\w+)`)
	type site struct{ file, fn, rest string }
	var found []site
	for _, dir := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // path comes from walking this repo's own source tree
			if err != nil {
				return err
			}
			src := string(b)
			for off := 0; ; {
				i := strings.Index(src[off:], stmt)
				if i < 0 {
					break
				}
				i += off
				off = i + len(stmt)
				fn := ""
				if f := strings.LastIndex(src[:i], "\nfunc "); f >= 0 {
					if m := funcName.FindStringSubmatch(src[f+1:]); m != nil {
						fn = m[1]
					}
				}
				rest := src[off:]
				rest = strings.TrimSpace(rest[:strings.IndexAny(rest, "`\"")])
				found = append(found, site{filepath.Base(path), fn, rest})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []site{
		{"embeddings.go", "DropRecordEmbeddings", ""},
		{"records.go", "tombstoneRecordTx", "WHERE record_id = ?"},
	}
	if !slices.Equal(found, want) {
		t.Fatalf("vector DELETEs = %+v\nwant only %+v: one Record's vector is deleted only inside tombstoneRecordTx, and the one other DELETE (embed --drop) removes every row", found, want)
	}
}

// embed --drop deletes every vector, and the secure_delete pragma plus a
// WAL checkpoint leave none of their bytes in the DB file or its WAL.
// Records are untouched.
func TestDropRecordEmbeddingsLeavesNoVectorOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backstory.db")
	s := mustOpen(t, path)
	const marker = "bsvecdropzqx0007"
	vec := make([]float32, len(marker)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32([]byte(marker[4*i:])))
	}
	a := insertFiller(t, s, "alpha keeps its text")
	b := insertFiller(t, s, "beta keeps its text")
	mustUpsertVector(t, s, a, testModel, vec)
	mustUpsertVector(t, s, b, testModel, vec)
	if rawMarkerCount(t, path, marker) == 0 {
		t.Fatal("precondition: the vector bytes should be on disk before --drop")
	}

	n, err := s.DropRecordEmbeddings()
	if err != nil || n != 2 {
		t.Fatalf("DropRecordEmbeddings = %d, %v; want 2", n, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM record_embeddings`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows left = %d, %v; want 0", rows, err)
	}
	if c := rawMarkerCount(t, path, marker); c != 0 {
		t.Fatalf("vector bytes occur %d times in db/-wal after --drop, want 0", c)
	}
	for _, id := range []string{a, b} {
		if r, err := s.GetRecord(id); err != nil || r.TombstonedAt != nil || !strings.Contains(r.Text, "keeps its text") {
			t.Errorf("record %s after --drop = %+v, %v; want it untouched", id, r, err)
		}
	}
}
