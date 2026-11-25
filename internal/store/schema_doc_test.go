package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// schemaDocPlanned names tables SCHEMA.md documents ahead of their migration.
// Remove an entry in the commit that adds the table's migration.
var schemaDocPlanned = map[string]bool{}

var (
	schemaDocCreate = regexp.MustCompile(`^CREATE TABLE ([a-z_][a-z0-9_]*) \(`)
	schemaDocColumn = regexp.MustCompile(`^\s+([a-z_][a-z0-9_]*)\s+(TEXT|INTEGER|BLOB|REAL|NUMERIC)\b`)
	schemaDocAlter  = regexp.MustCompile(`^ALTER TABLE ([a-z_][a-z0-9_]*)\s+ADD COLUMN ([a-z_][a-z0-9_]*)\s`)
)

// schemaDocTables parses SCHEMA.md's sql fences into table -> column set: one
// column per line inside each `CREATE TABLE`, plus `ALTER TABLE … ADD COLUMN`
// lines. Virtual tables (FTS5) are skipped: their shadow tables are SQLite's,
// not ours.
func schemaDocTables(t *testing.T) map[string]map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "SCHEMA.md"))
	if err != nil {
		t.Fatalf("read SCHEMA.md: %v", err)
	}
	tables := map[string]map[string]bool{}
	inSQL, table := false, ""
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "```sql"):
			inSQL = true
			continue
		case strings.HasPrefix(line, "```"):
			inSQL, table = false, ""
			continue
		}
		if !inSQL {
			continue
		}
		if m := schemaDocAlter.FindStringSubmatch(line); m != nil {
			if tables[m[1]] == nil {
				t.Fatalf("SCHEMA.md alters table %s before defining it", m[1])
			}
			tables[m[1]][m[2]] = true
			continue
		}
		if m := schemaDocCreate.FindStringSubmatch(line); m != nil {
			table = m[1]
			if tables[table] != nil {
				t.Fatalf("SCHEMA.md defines table %s twice", table)
			}
			tables[table] = map[string]bool{}
			continue
		}
		if table == "" {
			continue
		}
		if strings.HasPrefix(line, ");") {
			table = ""
			continue
		}
		if m := schemaDocColumn.FindStringSubmatch(line); m != nil {
			tables[table][m[1]] = true
		}
	}
	return tables
}

// TestSchemaDocMatchesMigrations keeps SCHEMA.md's DDL a true description of
// the store: every table and column a fully migrated store has must appear in
// SCHEMA.md, and nothing SCHEMA.md lists may be missing from the store, except
// tables in schemaDocPlanned.
func TestSchemaDocMatchesMigrations(t *testing.T) {
	doc := schemaDocTables(t)
	s := mustOpen(t, filepath.Join(t.TempDir(), "backstory.db"))

	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'records_fts%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var live []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		live = append(live, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("list tables: %v", err)
	}

	var drift []string
	seen := map[string]bool{}
	for _, table := range live {
		seen[table] = true
		cols, documented := doc[table]
		if !documented {
			drift = append(drift, "table "+table+" exists in the store but not in SCHEMA.md")
			continue
		}
		crows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		liveCols := map[string]bool{}
		for crows.Next() {
			var col string
			if err := crows.Scan(&col); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			liveCols[col] = true
			if !cols[col] {
				drift = append(drift, "column "+table+"."+col+" exists in the store but not in SCHEMA.md")
			}
		}
		if err := crows.Close(); err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		for col := range cols {
			if !liveCols[col] {
				drift = append(drift, "column "+table+"."+col+" is in SCHEMA.md but not in the store")
			}
		}
	}
	for table := range doc {
		if seen[table] {
			if schemaDocPlanned[table] {
				drift = append(drift, "table "+table+" is migrated now: remove it from schemaDocPlanned")
			}
			continue
		}
		if !schemaDocPlanned[table] {
			drift = append(drift, "table "+table+" is in SCHEMA.md but not in the store")
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		t.Errorf("SCHEMA.md has drifted from the migrations (%d):\n  %s", len(drift), strings.Join(drift, "\n  "))
	}
}
