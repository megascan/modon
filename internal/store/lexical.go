package store

import (
	"strings"
	"sync"

	"modon/internal/model"
	"modon/internal/naming"
)

// bm25Rank weighs FTS columns: title, name, description, category, style,
// tags, creator, name_words, pack.
const bm25Rank = `bm25(assets_fts, 12.0, 4.0, 3.0, 4.0, 3.0, 5.0, 2.0, 10.0, 1.5)`

// nameWords is the searchable split of a file-derived title:
// "SM_Prop_WallLantern_01" also matches "wall" and "lantern".
func nameWords(title string) string { return naming.CleanName(title) }

func (s *Store) fillNameWords() error {
	rows, err := s.db.Query(`SELECT id, title, path FROM assets`)
	if err != nil {
		return err
	}
	type row struct {
		id          int64
		words, pack string
	}
	var all []row
	for rows.Next() {
		var id int64
		var title, path string
		if err := rows.Scan(&id, &title, &path); err != nil {
			rows.Close()
			return err
		}
		all = append(all, row{id, nameWords(title), naming.PackName(path)})
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE assets SET name_words=?, pack=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, r := range all {
		if _, err := st.Exec(r.words, r.pack, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type dfCache struct {
	mu    sync.Mutex
	total int
	dv    int64
	df    map[string]int
}

// TokenDF returns, for each search token, how many assets match it
// (prefix match on the indexed text, like keyword search), plus the total
// asset count. Counts are cached until the index changes.
func (s *Store) TokenDF(tokens []string) (map[string]int, int, error) {
	var dv int64
	if err := s.db.QueryRow(`PRAGMA data_version`).Scan(&dv); err != nil {
		return nil, 0, err
	}
	total, err := s.Count()
	if err != nil {
		return nil, 0, err
	}
	s.df.mu.Lock()
	if s.df.df == nil || s.df.total != total || s.df.dv != dv {
		s.df.df, s.df.total, s.df.dv = map[string]int{}, total, dv
	}
	out := map[string]int{}
	var missing []string
	for _, t := range tokens {
		if n, ok := s.df.df[t]; ok {
			out[t] = n
		} else {
			missing = append(missing, t)
		}
	}
	s.df.mu.Unlock()
	for _, t := range missing {
		var n int
		if s.fts {
			err = s.db.QueryRow(`SELECT COUNT(*) FROM assets_fts WHERE assets_fts MATCH ?`, ftsToken(t)).Scan(&n)
		} else {
			l := "%" + t + "%"
			err = s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE LOWER(title) LIKE ? OR LOWER(tags) LIKE ? OR LOWER(category) LIKE ?`, l, l, l).Scan(&n)
		}
		if err != nil {
			return nil, 0, err
		}
		out[t] = n
		s.df.mu.Lock()
		s.df.df[t] = n
		s.df.mu.Unlock()
	}
	return out, total, nil
}

func ftsToken(t string) string { return `"` + strings.ReplaceAll(t, `"`, `""`) + `"*` }

// LexicalCandidates returns assets matching any of the tokens (prefix match
// on the indexed text), best BM25 first, up to limit.
func (s *Store) LexicalCandidates(tokens []string, allowedFilter model.Query, limit int) ([]model.Asset, error) {
	if !s.fts || len(tokens) == 0 {
		return nil, nil
	}
	parts := make([]string, len(tokens))
	for i, t := range tokens {
		parts[i] = ftsToken(t)
	}
	aliasWheres, wargs := queryStrings(allowedFilter, "a.")
	query := `SELECT ` + selectColsAlias + `, ` + bm25Rank + ` AS score
		FROM assets_fts JOIN assets a ON a.id = assets_fts.rowid
		WHERE assets_fts MATCH ?` + andClause(strings.Join(aliasWheres, " AND ")) + ` ORDER BY score LIMIT ?`
	rows, err := s.db.Query(query, append(append([]any{strings.Join(parts, " OR ")}, wargs...), limit)...)
	if err != nil {
		return nil, err
	}
	return collectFTS(rows), nil
}

// TokenMatch says how one asset matches one query token, by column group
// (Name: title, name and the split name words; Any: every indexed
// column): 2 = the whole (stemmed) word, 1 = only as a word prefix
// ("work" in "workbench"), 0 = not at all.
type TokenMatch struct{ Name, Any uint8 }

// TokenMatches reports, per token, how each of the given assets matches it,
// using the FTS tokenizer (porter stemming) like keyword search.
func (s *Store) TokenMatches(tokens []string, ids []int64) (map[string]map[int64]TokenMatch, error) {
	out := map[string]map[int64]TokenMatch{}
	if !s.fts || len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	for _, t := range tokens {
		m := map[int64]TokenMatch{}
		exact := `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		// Prefix first, then exact overwrites with the stronger level.
		for _, q := range []struct {
			expr  string
			name  bool
			level uint8
		}{
			{exact + "*", false, 1}, {"{title name name_words} : " + exact + "*", true, 1},
			{exact, false, 2}, {"{title name name_words} : " + exact, true, 2},
		} {
			rows, err := s.db.Query(`SELECT rowid FROM assets_fts WHERE assets_fts MATCH ? AND rowid IN (`+ph+`)`,
				append([]any{q.expr}, idArgs...)...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id int64
				if rows.Scan(&id) != nil {
					continue
				}
				tm := m[id]
				if q.name {
					tm.Name = q.level
				} else {
					tm.Any = q.level
				}
				m[id] = tm
			}
			rows.Close()
		}
		out[t] = m
	}
	return out, nil
}
