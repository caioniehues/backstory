package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Tunables for turning search text into an FTS5 query. Named here, never inlined.
const (
	// SearchShortTextTokens is the most whitespace-separated tokens text can
	// have and still be quoted token by token. Longer text is reduced to its
	// distinctive terms instead.
	SearchShortTextTokens = 4
	// SearchMinTermRunes is the shortest distinctive term kept from long
	// text: shorter words ("to", "the", "use") are noise that would make
	// every Record a match.
	SearchMinTermRunes = 4
	// SearchMaxTerms caps how many distinctive terms of long text feed the
	// query.
	SearchMaxTerms = 12
)

// notSuperseded is the condition over records r that r is the to_id of no
// supersedes edge: with r.tombstoned_at IS NULL, what makes a Record Current.
const notSuperseded = "NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = r.id AND e.type = 'supersedes')"

// SearchFilter narrows a search before anything is ranked, so a narrow
// filter never comes back short. Tombstoned Records and expired Drafts are
// always excluded.
type SearchFilter struct {
	// ProjectKey is the Project searched when Scope is nil.
	ProjectKey string
	// Scope, when set, searches a Work location's scope instead of ProjectKey.
	Scope *LocationScope
	// Kinds keeps only Records of these kinds; empty keeps every kind.
	Kinds []RecordKind
	// CurrentOnly also excludes superseded Records.
	CurrentOnly bool
	// Limit caps how many ids come back; zero or less returns none.
	Limit int
	// KeyTermsOnly reduces the text to its distinctive terms whatever its
	// length, instead of choosing the rule by length (searchTerms).
	KeyTermsOnly bool
}

// SearchText returns the ids of the Records matching text under f, best
// first. A Record matches when its text matches any of text's terms by full
// text (best bm25 first, ties newest first by ts). A Handoff whose next
// step contains a term, case-insensitively, also matches: those follow the
// full-text matches, newest first by sequence. Any text is safe: it never
// reaches FTS5's query parser except as quoted literals.
func (s *Store) SearchText(text string, f SearchFilter) ([]string, error) {
	if f.Limit <= 0 {
		return nil, nil
	}
	terms := searchTerms(text, f.KeyTermsOnly)
	if len(terms) == 0 {
		return nil, nil
	}
	where, args, ok := s.searchWhere(f)
	if !ok {
		return nil, nil
	}

	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	ftsArgs := append(append([]any{strings.Join(quoted, " OR ")}, args...), f.Limit)
	//nolint:gosec // where is assembled from fixed fragments and "?" placeholders; every value is bound
	rows, err := s.db.Query(`SELECT r.id FROM records_fts
		JOIN records r ON r.rowid = records_fts.rowid
		WHERE records_fts MATCH ? AND `+where+`
		ORDER BY rank, r.ts DESC LIMIT ?`, ftsArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: search text: %w", err)
	}
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("store: search text: %w", err)
	}
	if len(ids) >= f.Limit || !kindAllowed(f.Kinds, KindHandoff) {
		return ids, nil
	}

	byNext, err := s.handoffsByNext(terms, where, args)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	for _, id := range byNext {
		if len(ids) == f.Limit {
			break
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// handoffsByNext returns the ids of the Handoffs under where whose next step
// contains any of terms, case-insensitively, newest first by sequence.
// records_fts indexes only text, so next is matched here.
func (s *Store) handoffsByNext(terms []string, where string, args []any) ([]string, error) {
	//nolint:gosec // where is assembled from fixed fragments and "?" placeholders; every value is bound
	rows, err := s.db.Query(`SELECT r.id, r.next FROM records r
		WHERE r.kind = 'handoff' AND r.next IS NOT NULL AND r.next != '' AND `+where+`
		ORDER BY r.rowid DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search handoff next: %w", err)
	}
	defer func() { _ = rows.Close() }()
	lower := make([]string, len(terms))
	for i, t := range terms {
		lower[i] = strings.ToLower(t)
	}
	var ids []string
	for rows.Next() {
		var id, next string
		if err := rows.Scan(&id, &next); err != nil {
			return nil, fmt.Errorf("store: search handoff next: %w", err)
		}
		next = strings.ToLower(next)
		for _, t := range lower {
			if strings.Contains(next, t) {
				ids = append(ids, id)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search handoff next: %w", err)
	}
	return ids, nil
}

// searchWhere is f as a condition over records r, with its bound args. ok is
// false when f can match nothing (a scope with neither a repo key nor a
// session). Tombstoned Records and expired Drafts (ExpiredDraft) are always
// excluded.
func (s *Store) searchWhere(f SearchFilter) (where string, args []any, ok bool) {
	conds := []string{"r.tombstoned_at IS NULL", notExpiredDraft}
	args = append(args, tsToNanos(s.now()))
	if f.Scope != nil {
		var either []string
		if f.Scope.RepoKey != "" {
			either = append(either, "r.project_key = ?")
			args = append(args, f.Scope.RepoKey)
		}
		if ids := f.Scope.sortedSessionIDs(); len(ids) > 0 {
			either = append(either, "r.session_id IN (SELECT value FROM json_each(?))")
			args = append(args, jsonArray(ids))
		}
		if len(either) == 0 {
			return "", nil, false
		}
		conds = append(conds, "("+strings.Join(either, " OR ")+")")
	} else {
		conds = append(conds, "r.project_key = ?")
		args = append(args, s.canonicalizeProjectKey(f.ProjectKey))
	}
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		conds = append(conds, "r.kind IN (SELECT value FROM json_each(?))")
		args = append(args, jsonArray(kinds))
	}
	if f.CurrentOnly {
		conds = append(conds, notSuperseded)
	}
	return strings.Join(conds, " AND "), args, true
}

// searchTerms is the rule turning search text into terms, chosen by length
// unless keyTermsOnly. Short text (at most SearchShortTextTokens tokens)
// keeps every whitespace-separated token verbatim, so `wobble-party` and
// hashes match exactly. Long text, or any text when keyTermsOnly, keeps only
// its distinctive terms: runs of letters and digits of at least
// SearchMinTermRunes, lower-cased, deduplicated, at most SearchMaxTerms.
func searchTerms(text string, keyTermsOnly bool) []string {
	fields := strings.Fields(text)
	if !keyTermsOnly && len(fields) <= SearchShortTextTokens {
		return fields
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var terms []string
	seen := map[string]bool{}
	for _, w := range words {
		if len([]rune(w)) < SearchMinTermRunes || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, w)
		if len(terms) == SearchMaxTerms {
			break
		}
	}
	return terms
}

func kindAllowed(kinds []RecordKind, k RecordKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, want := range kinds {
		if want == k {
			return true
		}
	}
	return false
}

// jsonArray encodes strs for a json_each(?) binding.
func jsonArray(strs []string) string {
	b, _ := json.Marshal(strs) // a []string always marshals
	return string(b)
}

// DecisionsSharingAbout returns the ids of the current Decisions in
// projectKey sharing at least one of about's paths: most shared paths first,
// then newest. It is the about half of the related-Decisions check on a new
// Decision; the text half is a search.
func (s *Store) DecisionsSharingAbout(projectKey string, about []string) ([]string, error) {
	if len(about) == 0 {
		return nil, nil
	}
	want := make(map[string]bool, len(about))
	for _, a := range about {
		want[a] = true
	}
	rows, err := s.db.Query(`SELECT r.id, r.about FROM records r
		WHERE r.kind = 'decision' AND r.tombstoned_at IS NULL AND r.project_key = ? AND r.about IS NOT NULL
		AND `+notSuperseded+`
		ORDER BY r.ts DESC`, s.canonicalizeProjectKey(projectKey))
	if err != nil {
		return nil, fmt.Errorf("store: decisions sharing about: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type hit struct {
		id     string
		shared int
	}
	var hits []hit
	for rows.Next() {
		var id, aboutJSON string
		if err := rows.Scan(&id, &aboutJSON); err != nil {
			return nil, fmt.Errorf("store: decisions sharing about: %w", err)
		}
		var paths []string
		if json.Unmarshal([]byte(aboutJSON), &paths) != nil {
			continue
		}
		n := 0
		for _, p := range paths {
			if want[p] {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, hit{id, n})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: decisions sharing about: %w", err)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].shared > hits[j].shared })
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids, nil
}
