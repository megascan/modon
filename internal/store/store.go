// Package store implements the local SQLite asset index with full-text
// search (FTS5 when available, weighted LIKE fallback otherwise).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modon/internal/model"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	path string
	fts  bool
	vc   vecCache
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc sqlite is happiest with a single connection; the index is not
	// write-concurrent.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path is the database file location.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	q := `CREATE TABLE IF NOT EXISTS assets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT NOT NULL UNIQUE,
		title TEXT NOT NULL,
		name TEXT DEFAULT '',
		description TEXT DEFAULT '',
		asset_type TEXT DEFAULT 'model',
		category TEXT DEFAULT '',
		style TEXT DEFAULT '',
		tags TEXT DEFAULT '',
		animated INTEGER DEFAULT 0,
		animation_names TEXT DEFAULT '',
		tri_count INTEGER DEFAULT 0,
		vert_count INTEGER DEFAULT 0,
		formats TEXT DEFAULT '',
		related_files TEXT DEFAULT '',
		file_size INTEGER DEFAULT 0,
		texture_count INTEGER DEFAULT 0,
		material_names TEXT DEFAULT '',
		license TEXT DEFAULT '',
		creator TEXT DEFAULT '',
		preview_path TEXT DEFAULT '',
		preview_status TEXT DEFAULT 'none',
		needs_review INTEGER DEFAULT 1,
		imported_at TEXT DEFAULT '',
		updated_at TEXT DEFAULT ''
	)`
	if _, err := s.db.Exec(q); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.migrateVectors(); err != nil {
		return fmt.Errorf("migrate vectors: %w", err)
	}
	fts := `CREATE VIRTUAL TABLE IF NOT EXISTS assets_fts USING fts5(
		title, name, description, category, style, tags, creator,
		content='assets', content_rowid='id', tokenize='porter unicode61')`
	if _, err := s.db.Exec(fts); err != nil {
		return nil // FTS5 unavailable; LIKE fallback covers search
	}
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS assets_fts_ai AFTER INSERT ON assets BEGIN
			INSERT INTO assets_fts(rowid,title,name,description,category,style,tags,creator)
			VALUES (new.id,new.title,new.name,new.description,new.category,new.style,new.tags,new.creator);
		END`,
		`CREATE TRIGGER IF NOT EXISTS assets_fts_ad AFTER DELETE ON assets BEGIN
			INSERT INTO assets_fts(assets_fts,rowid,title,name,description,category,style,tags,creator)
			VALUES ('delete',old.id,old.title,old.name,old.description,old.category,old.style,old.tags,old.creator);
		END`,
		`CREATE TRIGGER IF NOT EXISTS assets_fts_au AFTER UPDATE ON assets BEGIN
			INSERT INTO assets_fts(assets_fts,rowid,title,name,description,category,style,tags,creator)
			VALUES ('delete',old.id,old.title,old.name,old.description,old.category,old.style,old.tags,old.creator);
			INSERT INTO assets_fts(rowid,title,name,description,category,style,tags,creator)
			VALUES (new.id,new.title,new.name,new.description,new.category,new.style,new.tags,new.creator);
		END`,
	}
	for _, t := range triggers {
		if _, err := s.db.Exec(t); err != nil {
			return nil
		}
	}
	s.fts = true
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func joinComma(ss []string) string { return strings.Join(ss, ", ") }

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ", ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func mergeUnique(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, v := range append(append([]string{}, a...), b...) {
		k := strings.ToLower(v)
		if v == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}

// Upsert inserts or updates the local asset identified by its path and
// returns the row id. Agent-written metadata is preserved across re-imports.
func (s *Store) Upsert(a model.Asset) (int64, error) {
	if a.PrimaryFile == "" {
		return 0, errors.New("asset has no primary file")
	}
	abs, err := filepath.Abs(a.PrimaryFile)
	if err != nil {
		abs = a.PrimaryFile
	}
	a.PrimaryFile = abs

	var prevDesc, prevCat, prevStyle, prevType, prevTags string
	var prevNeeds int
	err = s.db.QueryRow(`SELECT description, category, style, asset_type, tags, needs_review
		FROM assets WHERE path=?`, a.PrimaryFile).
		Scan(&prevDesc, &prevCat, &prevStyle, &prevType, &prevTags, &prevNeeds)
	existed := true
	if errors.Is(err, sql.ErrNoRows) {
		existed = false
	} else if err != nil {
		return 0, err
	}
	if prevDesc != "" {
		a.Description = prevDesc
	}
	if prevCat != "" {
		a.Category = prevCat
	}
	if prevStyle != "" {
		a.Style = prevStyle
	}
	if prevType != "" && prevType != "model" {
		a.AssetType = prevType
	}
	if prevTags != "" {
		a.Tags = mergeUnique(strings.Fields(prevTags), a.Tags)
	}
	needs := boolInt(a.Description == "")
	if existed && prevNeeds == 0 {
		needs = 0
	}

	res, err := s.db.Exec(`INSERT INTO assets
		(path,title,name,description,asset_type,category,style,tags,animated,animation_names,
		 tri_count,vert_count,formats,related_files,file_size,texture_count,material_names,
		 license,creator,preview_path,preview_status,needs_review,imported_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			title=excluded.title, name=excluded.name,
			description=excluded.description, asset_type=excluded.asset_type,
			category=excluded.category, style=excluded.style, tags=excluded.tags,
			animated=excluded.animated, animation_names=excluded.animation_names,
			tri_count=excluded.tri_count, vert_count=excluded.vert_count,
			formats=excluded.formats, related_files=excluded.related_files,
			file_size=excluded.file_size, texture_count=excluded.texture_count,
			material_names=excluded.material_names, license=excluded.license,
			creator=excluded.creator, needs_review=excluded.needs_review,
			updated_at=excluded.updated_at`,
		a.PrimaryFile, a.Title, a.Name, a.Description, a.AssetType, a.Category, a.Style,
		strings.Join(a.Tags, " "), boolInt(a.Animated), joinComma(a.AnimationNames),
		a.TriCount, a.VertCount, joinComma(a.Formats), joinComma(a.Files), a.FileSize,
		a.TextureCount, joinComma(a.MaterialNames), a.License, a.Creator,
		a.PreviewPath, previewStatus(a), needs, now(), now())
	if err != nil {
		return 0, err
	}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		return id, nil
	}
	var id int64
	err = s.db.QueryRow(`SELECT id FROM assets WHERE path=?`, a.PrimaryFile).Scan(&id)
	return id, err
}

func previewStatus(a model.Asset) string {
	if a.PreviewPath != "" {
		return "ok"
	}
	return "none"
}

const selectCols = `id,path,title,name,description,asset_type,category,style,tags,
	animated,animation_names,tri_count,vert_count,formats,related_files,file_size,
	texture_count,material_names,license,creator,preview_path,preview_status,needs_review,
	imported_at,updated_at`

// selectColsAlias qualifies every column for joined queries (FTS + assets).
const selectColsAlias = `a.id,a.path,a.title,a.name,a.description,a.asset_type,a.category,a.style,a.tags,
	a.animated,a.animation_names,a.tri_count,a.vert_count,a.formats,a.related_files,a.file_size,
	a.texture_count,a.material_names,a.license,a.creator,a.preview_path,a.preview_status,a.needs_review,
	a.imported_at,a.updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanAsset(sc scanner, withScore bool) (model.Asset, float64, error) {
	var a model.Asset
	var score float64
	var path, name, desc, atype, category, style, tags string
	var animated, needs int
	var animNames, formats, related, materials string
	var license, creator, previewPath, previewStatus, importedAt, updatedAt string

	dest := []any{&a.RowID, &path, &a.Title, &name, &desc, &atype, &category, &style, &tags,
		&animated, &animNames, &a.TriCount, &a.VertCount, &formats, &related, &a.FileSize,
		&a.TextureCount, &materials, &license, &creator, &previewPath, &previewStatus, &needs,
		&importedAt, &updatedAt}
	if withScore {
		dest = append(dest, &score)
	}
	if err := sc.Scan(dest...); err != nil {
		return a, 0, err
	}

	a.Source = model.SourceLocal
	a.PrimaryFile = path
	a.Name = name
	a.Description = desc
	a.AssetType = orDefault(atype, "model")
	a.Category = category
	a.Style = style
	a.Tags = strings.Fields(tags)
	a.Animated = animated == 1
	a.AnimationNames = splitComma(animNames)
	a.Formats = splitComma(formats)
	a.Files = splitComma(related)
	a.MaterialNames = splitComma(materials)
	a.License = license
	a.Creator = creator
	a.PreviewPath = previewPath
	a.IsFree = true
	a.Owned = true
	a.NeedsReview = needs == 1
	a.ID = fmt.Sprintf("local:%d", a.RowID)
	return a, score, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (s *Store) GetByID(id int64) (model.Asset, error) {
	row := s.db.QueryRow(`SELECT `+selectCols+` FROM assets WHERE id=?`, id)
	a, _, err := scanAsset(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("no local asset with id %d", id)
	}
	return a, err
}

func (s *Store) GetByPath(path string) (model.Asset, error) {
	row := s.db.QueryRow(`SELECT `+selectCols+` FROM assets WHERE path=?`, path)
	a, _, err := scanAsset(row, false)
	return a, err
}

// ExistsByPath reports whether an asset with this path is already indexed.
func (s *Store) ExistsByPath(path string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM assets WHERE path=?`, path).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// queryStrings builds facet filters. prefix qualifies columns so the same
// filter works both on plain `assets` ("") and on the alias `a` ("a.").
func queryStrings(q model.Query, prefix string) (wheres []string, args []any) {
	if q.AssetType != "" {
		wheres = append(wheres, prefix+"asset_type = ?")
		args = append(args, q.AssetType)
	}
	if q.Category != "" {
		like := "%" + strings.ToLower(q.Category) + "%"
		wheres = append(wheres, "(LOWER("+prefix+"category) LIKE ? OR LOWER("+prefix+"tags) LIKE ? OR LOWER("+prefix+"title) LIKE ?)")
		args = append(args, like, like, like)
	}
	if q.Style != "" {
		like := "%" + strings.ToLower(q.Style) + "%"
		wheres = append(wheres, "(LOWER("+prefix+"style) LIKE ? OR LOWER("+prefix+"tags) LIKE ?)")
		args = append(args, like, like)
	}
	if q.Animated != nil {
		wheres = append(wheres, prefix+"animated = ?")
		args = append(args, boolInt(*q.Animated))
	}
	for _, t := range q.Tags {
		like := "%" + strings.ToLower(t) + "%"
		wheres = append(wheres, "(LOWER("+prefix+"tags) LIKE ? OR LOWER("+prefix+"title) LIKE ? OR LOWER("+prefix+"description) LIKE ?)")
		args = append(args, like, like, like)
	}
	return wheres, args
}

// Keyword match modes reported by SearchMode, strictest first.
const (
	MatchAll  = "all"  // every query token matched (FTS AND)
	MatchAny  = "any"  // loose fallback: any token (FTS OR) or substring LIKE
	MatchNone = "none" // no text query: plain filtered listing
)

// Search runs a weighted full-text (or LIKE fallback) search with optional
// facet filters. Local assets are always free, so price filters are ignored.
func (s *Store) Search(q model.Query) ([]model.Asset, error) {
	assets, _, err := s.SearchMode(q)
	return assets, err
}

// SearchMode is Search that also reports how strictly the text matched, so
// hybrid ranking can trust loose fallback matches less.
func (s *Store) SearchMode(q model.Query) ([]model.Asset, string, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 12
	}
	wheres, wargs := queryStrings(q, "")
	aliasWheres, _ := queryStrings(q, "a.")
	aliasFilter := strings.Join(aliasWheres, " AND ")
	filter := strings.Join(wheres, " AND ")

	tokens := searchTokens(q.Text)
	var assets []model.Asset
	mode := MatchNone

	if s.fts && len(tokens) > 0 {
		for i, match := range ftsQueries(tokens) {
			query := `SELECT ` + selectColsAlias + `, bm25(assets_fts, 12.0, 4.0, 3.0, 4.0, 3.0, 5.0, 2.0) AS score
				FROM assets_fts JOIN assets a ON a.id = assets_fts.rowid
				WHERE assets_fts MATCH ?` + andClause(aliasFilter) + ` ORDER BY score LIMIT ?`
			rows, err := s.db.Query(query, append([]any{match}, append(wargs, limit)...)...)
			if err != nil {
				return nil, "", err
			}
			assets = collectFTS(rows)
			if len(assets) > 0 {
				mode = MatchAny
				if i == 0 || len(tokens) == 1 {
					mode = MatchAll
				}
				break
			}
		}
	}
	if len(assets) == 0 {
		if len(tokens) > 0 {
			mode = MatchAny
			var likes []string
			var largs []any
			for _, t := range tokens {
				likes = append(likes, `(LOWER(title) LIKE ? OR LOWER(name) LIKE ? OR LOWER(description) LIKE ?
					OR LOWER(category) LIKE ? OR LOWER(style) LIKE ? OR LOWER(tags) LIKE ?
					OR LOWER(material_names) LIKE ? OR LOWER(path) LIKE ?)`)
				l := "%" + t + "%"
				for i := 0; i < 8; i++ {
					largs = append(largs, l)
				}
			}
			query := `SELECT ` + selectCols + ` FROM assets WHERE (` + strings.Join(likes, " AND ") + `)` + andClause(filter) + ` LIMIT ?`
			rows, err := s.db.Query(query, append(largs, append(wargs, limit)...)...)
			if err != nil {
				return nil, "", err
			}
			assets = collect(rows)
		} else {
			query := `SELECT ` + selectCols + ` FROM assets` + whereClause(filter) + ` ORDER BY id LIMIT ?`
			rows, err := s.db.Query(query, append(wargs, limit)...)
			if err != nil {
				return nil, "", err
			}
			assets = collect(rows)
		}
	}

	// Rank exact-substring title matches first.
	if q.Text != "" {
		lq := strings.ToLower(q.Text)
		sort.SliceStable(assets, func(i, j int) bool {
			mi := strings.Contains(strings.ToLower(assets[i].Title), lq)
			mj := strings.Contains(strings.ToLower(assets[j].Title), lq)
			if mi != mj {
				return mi
			}
			return assets[i].RowID < assets[j].RowID
		})
	}
	return assets, mode, nil
}

func andClause(cond string) string {
	if cond == "" {
		return ""
	}
	return " AND " + cond
}

func whereClause(cond string) string {
	if cond == "" {
		return ""
	}
	return " WHERE " + cond
}

func collectFTS(rows *sql.Rows) []model.Asset {
	defer rows.Close()
	var out []model.Asset
	for rows.Next() {
		a, _, err := scanAsset(rows, true)
		if err == nil {
			out = append(out, a)
		}
	}
	return out
}

func collect(rows *sql.Rows) []model.Asset {
	defer rows.Close()
	var out []model.Asset
	for rows.Next() {
		a, _, err := scanAsset(rows, false)
		if err == nil {
			out = append(out, a)
		}
	}
	return out
}

// UpdateMetadata stores agent-written enrichment and clears needs_review
// once a description exists. Empty arguments leave the field unchanged.
func (s *Store) UpdateMetadata(id int64, description, category, style, assetType string, tags []string) (model.Asset, error) {
	a, err := s.GetByID(id)
	if err != nil {
		return a, err
	}
	if description != "" {
		a.Description = description
	}
	if category != "" {
		a.Category = category
	}
	if style != "" {
		a.Style = style
	}
	if assetType != "" {
		a.AssetType = assetType
	}
	if len(tags) > 0 {
		a.Tags = mergeUnique(a.Tags, tags)
	}
	a.NeedsReview = a.Description == ""
	_, err = s.db.Exec(`UPDATE assets SET description=?, category=?, style=?, asset_type=?, tags=?, needs_review=?, updated_at=? WHERE id=?`,
		a.Description, a.Category, a.Style, a.AssetType, strings.Join(a.Tags, " "), boolInt(a.NeedsReview), now(), id)
	if err != nil {
		return a, err
	}
	return s.GetByID(id)
}

func (s *Store) ListUnclassified(limit int) ([]model.Asset, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT `+selectCols+` FROM assets WHERE needs_review=1 ORDER BY file_size DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []model.Asset
	for rows.Next() {
		a, _, err := scanAsset(rows, false)
		if err == nil {
			out = append(out, a)
		}
	}
	rows.Close()
	return out, nil
}

func (s *Store) SetPreview(id int64, path, status string) error {
	_, err := s.db.Exec(`UPDATE assets SET preview_path=?, preview_status=?, updated_at=? WHERE id=?`,
		path, status, now(), id)
	return err
}

func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&n)
	return n, err
}

func (s *Store) Stats() (map[string]any, error) {
	out := map[string]any{}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&total); err != nil {
		return nil, err
	}
	out["total"] = total
	var unclassified int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE needs_review=1`).Scan(&unclassified)
	out["needs_review"] = unclassified
	out["fts_enabled"] = s.fts
	for _, dim := range []string{"asset_type", "category", "style"} {
		rows, err := s.db.Query(`SELECT ` + dim + `, COUNT(*) FROM assets GROUP BY ` + dim + ` ORDER BY COUNT(*) DESC LIMIT 15`)
		if err != nil {
			continue
		}
		m := map[string]int{}
		for rows.Next() {
			var k string
			var v int
			if err := rows.Scan(&k, &v); err == nil && k != "" {
				m[k] = v
			}
		}
		rows.Close()
		out[dim+"s"] = m
	}
	var animated int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE animated=1`).Scan(&animated)
	out["animated"] = animated
	return out, nil
}

func searchTokens(text string) []string {
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		isAlnum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		return !isAlnum && r < 128
	})
	var out []string
	for _, t := range f {
		if len(t) > 1 {
			out = append(out, t)
		}
	}
	return out
}

// ftsQueries returns progressively looser FTS MATCH expressions.
func ftsQueries(tokens []string) []string {
	quote := func(t string) string { return `"` + strings.ReplaceAll(t, `"`, `""`) + `"` }
	ands := make([]string, len(tokens))
	for i, t := range tokens {
		ands[i] = quote(t) + `*`
	}
	return []string{strings.Join(ands, " AND "), strings.Join(ands, " OR ")}
}
