package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modon/internal/model"
)

// schemaVersion is stored in PRAGMA user_version.
//
//	1: assets, assets_fts, asset_vectors
//	2: per-field provenance (desc_src, category_src, style_src, type_src,
//	   tags_src), cluster_id, the claims table; the FTS update trigger
//	   fires only for indexed columns
const schemaVersion = 2

// provenanceFields are the Asset.Provenance keys, in column order
// (desc_src, category_src, style_src, type_src, tags_src).
var provenanceFields = [...]string{"description", "category", "style", "asset_type", "tags"}

func (s *Store) migrateSchema() error {
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&s.version); err != nil {
		return err
	}
	have := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(assets)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	for _, c := range []struct{ name, def string }{
		{"desc_src", "TEXT DEFAULT ''"},
		{"category_src", "TEXT DEFAULT ''"},
		{"style_src", "TEXT DEFAULT ''"},
		{"type_src", "TEXT DEFAULT ''"},
		{"tags_src", "TEXT DEFAULT ''"},
		{"cluster_id", "INTEGER DEFAULT 0"},
	} {
		if have[c.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE assets ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS claims (
			asset_id INTEGER PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
			worker TEXT NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS claims_worker ON claims(worker)`,
		`CREATE INDEX IF NOT EXISTS assets_cluster ON assets(cluster_id)`,
		`CREATE INDEX IF NOT EXISTS assets_review ON assets(needs_review, file_size)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	if s.version < 2 {
		// Every description in a v1 index was written through
		// update_asset_metadata (importers never set one).
		if _, err := s.db.Exec(`UPDATE assets SET desc_src='agent' WHERE description != '' AND desc_src = ''`); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS assets_fts_au`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) setVersion() error {
	if s.version >= schemaVersion {
		return nil
	}
	if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	s.version = schemaVersion
	return nil
}

// reviewed reports whether metadata is complete enough to leave the review
// queue: a description, or an agent-written category plus agent tags.
func reviewed(desc, category string, tags []string, src map[string]string) bool {
	if strings.TrimSpace(desc) != "" {
		return true
	}
	return category != "" && len(tags) > 0 &&
		src["category"] == model.ProvenanceAgent && src["tags"] == model.ProvenanceAgent
}

// normalizeTag makes a tag one token: tags are stored space-separated, so
// "work clothes" becomes "work-clothes" (FTS still matches both words).
func normalizeTag(t string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(t)), "-")
}

func joinTags(tags []string) string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t = normalizeTag(t); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(mergeUnique(out, nil), " ")
}

// Tag edit modes for MetadataUpdate.TagsMode.
const (
	TagsMerge   = "merge"
	TagsReplace = "replace"
)

// MetadataUpdate is an agent edit. Empty strings leave a field unchanged.
type MetadataUpdate struct {
	Description string
	Category    string
	Style       string
	AssetType   string
	Tags        []string
	// TagsMode is "merge" (default: add Tags to the current list) or
	// "replace" (Tags becomes the whole list; an empty list clears it).
	TagsMode   string
	RemoveTags []string // removed after merging/replacing, case-insensitive
	// Reviewed forces the review flag; nil derives it (a description, or an
	// agent category plus agent tags, clears needs_review).
	Reviewed *bool
}

func (u MetadataUpdate) touchesTags() bool {
	return len(u.Tags) > 0 || len(u.RemoveTags) > 0 || u.TagsMode == TagsReplace
}

// Empty reports whether the update would change nothing.
func (u MetadataUpdate) Empty() bool {
	return u.Description == "" && u.Category == "" && u.Style == "" && u.AssetType == "" &&
		!u.touchesTags() && u.Reviewed == nil
}

// UpdateMetadata applies an agent edit, records the edited fields as
// agent-written (so re-imports keep them) and releases any claim on the
// asset.
func (s *Store) UpdateMetadata(id int64, u MetadataUpdate) (model.Asset, error) {
	switch u.TagsMode {
	case "", TagsMerge, TagsReplace:
	default:
		return model.Asset{}, fmt.Errorf("tags_mode must be %q or %q, got %q", TagsMerge, TagsReplace, u.TagsMode)
	}
	a, err := s.GetByID(id)
	if err != nil {
		return a, err
	}
	src := map[string]string{}
	for k, v := range a.Provenance {
		src[k] = v
	}
	set := func(field, val string, dst *string) {
		if val = strings.TrimSpace(val); val != "" {
			*dst = val
			src[field] = model.ProvenanceAgent
		}
	}
	set("description", u.Description, &a.Description)
	set("category", u.Category, &a.Category)
	set("style", u.Style, &a.Style)
	set("asset_type", u.AssetType, &a.AssetType)
	if u.touchesTags() {
		add := make([]string, 0, len(u.Tags))
		for _, t := range u.Tags {
			if t = normalizeTag(t); t != "" {
				add = append(add, t)
			}
		}
		if u.TagsMode == TagsReplace {
			a.Tags = mergeUnique(add, nil)
		} else {
			a.Tags = mergeUnique(a.Tags, add)
		}
		if len(u.RemoveTags) > 0 {
			drop := map[string]bool{}
			for _, t := range u.RemoveTags {
				drop[strings.ToLower(normalizeTag(t))] = true
			}
			kept := a.Tags[:0]
			for _, t := range a.Tags {
				if !drop[strings.ToLower(t)] {
					kept = append(kept, t)
				}
			}
			a.Tags = kept
		}
		src["tags"] = model.ProvenanceAgent
	}
	needs := a.NeedsReview && !reviewed(a.Description, a.Category, a.Tags, src)
	if u.Reviewed != nil {
		needs = !*u.Reviewed
	}
	_, err = s.db.Exec(`UPDATE assets SET description=?, category=?, style=?, asset_type=?, tags=?, needs_review=?,
		desc_src=?, category_src=?, style_src=?, type_src=?, tags_src=?, updated_at=? WHERE id=?`,
		a.Description, a.Category, a.Style, a.AssetType, joinTags(a.Tags), boolInt(needs),
		src["description"], src["category"], src["style"], src["asset_type"], src["tags"], now(), id)
	if err != nil {
		return a, err
	}
	if _, err := s.db.Exec(`DELETE FROM claims WHERE asset_id=?`, id); err != nil {
		return a, err
	}
	return s.GetByID(id)
}

// SetClassification fills category/style where the agent or importer left
// them empty and records them as zero-shot ("auto") values.
func (s *Store) SetClassification(id int64, category, style string) error {
	_, err := s.db.Exec(`UPDATE assets SET
		category_src = CASE WHEN category = '' AND ? != '' THEN 'auto' ELSE category_src END,
		style_src = CASE WHEN style = '' AND ? != '' THEN 'auto' ELSE style_src END,
		category = CASE WHEN category = '' THEN ? ELSE category END,
		style = CASE WHEN style = '' THEN ? ELSE style END,
		updated_at = ? WHERE id = ?`, category, style, category, style, now(), id)
	return err
}

// ---- review queue ----

// UnclassifiedOptions pages and partitions the review queue.
type UnclassifiedOptions struct {
	Limit  int
	Offset int
	// Worker, when set, claims the returned assets for this worker id until
	// Lease expires, so parallel agents get disjoint batches. Assets claimed
	// by anyone (including this worker) are skipped until the claim expires
	// or update_asset_metadata releases it.
	Worker string
	Lease  time.Duration // default 15 minutes
	// OnePerCluster returns one representative per duplicate cluster (with
	// Duplicates listing the other copies still in review); with Worker the
	// whole cluster is claimed.
	OnePerCluster bool
}

// UnclassifiedPage is one page of the review queue.
type UnclassifiedPage struct {
	Assets         []model.Asset `json:"assets"`
	Remaining      int           `json:"remaining"` // unclaimed assets still needing review
	Worker         string        `json:"worker,omitempty"`
	LeaseExpiresAt string        `json:"lease_expires_at,omitempty"`
}

// DefaultLease is how long a claim lasts when no lease is given.
const DefaultLease = 15 * time.Minute

// ListUnclassified returns assets that still need review, largest files
// first. Assets under someone's live claim are skipped.
func (s *Store) ListUnclassified(o UnclassifiedOptions) (*UnclassifiedPage, error) {
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	if o.Lease <= 0 {
		o.Lease = DefaultLease
	}
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	claiming := o.Worker != ""
	if claiming {
		// IMMEDIATE takes the write lock up front, so two processes cannot
		// both select the same unclaimed rows.
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			return nil, err
		}
		defer conn.ExecContext(ctx, `ROLLBACK`) // no-op after COMMIT
	}
	nowUnix := time.Now().Unix()
	if _, err := conn.ExecContext(ctx, `DELETE FROM claims WHERE expires_at <= ?`, nowUnix); err != nil {
		return nil, err
	}
	free := `a.needs_review = 1 AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.asset_id = a.id)`
	where := free
	if o.OnePerCluster {
		where += ` AND (a.cluster_id = 0 OR a.id = (SELECT MIN(b.id) FROM assets b
			WHERE b.cluster_id = a.cluster_id AND b.needs_review = 1
			AND NOT EXISTS (SELECT 1 FROM claims c WHERE c.asset_id = b.id)))`
	}
	rows, err := conn.QueryContext(ctx, `SELECT `+selectColsAlias+` FROM assets a WHERE `+where+`
		ORDER BY a.file_size DESC, a.id LIMIT ? OFFSET ?`, o.Limit, o.Offset)
	if err != nil {
		return nil, err
	}
	assets := collect(rows)
	page := &UnclassifiedPage{Assets: assets}

	if o.OnePerCluster {
		for i, a := range assets {
			if a.ClusterID == 0 {
				continue
			}
			ids, err := clusterMembers(ctx, conn, a.ClusterID, true)
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if id != a.RowID {
					assets[i].Duplicates = append(assets[i].Duplicates, fmt.Sprintf("local:%d", id))
				}
			}
		}
	}
	if claiming {
		exp := time.Now().Add(o.Lease)
		for _, a := range assets {
			ids := []int64{a.RowID}
			if o.OnePerCluster {
				for _, d := range a.Duplicates {
					var id int64
					fmt.Sscanf(d, "local:%d", &id)
					ids = append(ids, id)
				}
			}
			for _, id := range ids {
				if _, err := conn.ExecContext(ctx, `INSERT INTO claims (asset_id, worker, expires_at) VALUES (?,?,?)
					ON CONFLICT(asset_id) DO UPDATE SET worker=excluded.worker, expires_at=excluded.expires_at`,
					id, o.Worker, exp.Unix()); err != nil {
					return nil, err
				}
			}
		}
		page.Worker = o.Worker
		page.LeaseExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets a WHERE `+free).Scan(&page.Remaining); err != nil {
		return nil, err
	}
	if claiming {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return nil, err
		}
	}
	return page, nil
}

// ReleaseClaims drops a worker's claims (all of them when ids is empty) and
// returns how many were released.
func (s *Store) ReleaseClaims(worker string, ids ...int64) (int, error) {
	if worker == "" {
		return 0, errors.New("worker id is required")
	}
	q := `DELETE FROM claims WHERE worker = ?`
	args := []any{worker}
	if len(ids) > 0 {
		q += ` AND asset_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ActiveClaims counts live claims per worker.
func (s *Store) ActiveClaims() map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT worker, COUNT(*) FROM claims WHERE expires_at > ? GROUP BY worker`, time.Now().Unix())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var w string
		var n int
		if rows.Scan(&w, &n) == nil {
			out[w] = n
		}
	}
	return out
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func clusterMembers(ctx context.Context, q querier, cluster int64, needsReviewOnly bool) ([]int64, error) {
	query := `SELECT id FROM assets WHERE cluster_id = ?`
	if needsReviewOnly {
		query += ` AND needs_review = 1`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY id`, cluster)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
