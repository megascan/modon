package store

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"

	"modon/internal/model"
)

// Vector kinds stored per asset.
const (
	VecText  = "text"  // title/description/tags/path document
	VecImage = "image" // rendered or embedded preview thumbnail
)

func (s *Store) migrateVectors() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS asset_vectors (
		asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		kind TEXT NOT NULL,
		model TEXT NOT NULL,
		dims INTEGER NOT NULL,
		hash TEXT NOT NULL,
		vec BLOB NOT NULL,
		updated_at TEXT DEFAULT '',
		PRIMARY KEY (asset_id, kind)
	)`)
	return err
}

// Matrix is an in-memory, row-major copy of one vector kind, used for
// brute-force cosine search (vectors are L2-normalized).
type Matrix struct {
	IDs  []int64
	Vecs []float32 // len(IDs) * Dims
	Dims int
	pos  map[int64]int
}

// Row returns the vector for an asset id, or nil.
func (m *Matrix) Row(id int64) []float32 {
	if m == nil {
		return nil
	}
	i, ok := m.pos[id]
	if !ok {
		return nil
	}
	return m.Vecs[i*m.Dims : (i+1)*m.Dims]
}

type vecCache struct {
	mu   sync.Mutex
	gen  uint64
	mats map[string]*cachedMatrix
}

type cachedMatrix struct {
	gen uint64
	m   *Matrix
}

func (s *Store) bumpVectors() {
	s.vc.mu.Lock()
	s.vc.gen++
	s.vc.mu.Unlock()
}

// PutVector stores (or replaces) one asset's vector of the given kind.
func (s *Store) PutVector(id int64, kind, modelID, hash string, v []float32) error {
	_, err := s.db.Exec(`INSERT INTO asset_vectors (asset_id,kind,model,dims,hash,vec,updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(asset_id,kind) DO UPDATE SET model=excluded.model, dims=excluded.dims,
			hash=excluded.hash, vec=excluded.vec, updated_at=excluded.updated_at`,
		id, kind, modelID, len(v), hash, encodeVec(v), now())
	if err == nil {
		s.bumpVectors()
	}
	return err
}

// VectorHashes returns asset id -> content hash for vectors that match the
// model and dimension, so callers can skip unchanged assets.
func (s *Store) VectorHashes(kind, modelID string, dims int) (map[int64]string, error) {
	rows, err := s.db.Query(`SELECT asset_id, hash FROM asset_vectors WHERE kind=? AND model=? AND dims=?`, kind, modelID, dims)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err == nil {
			out[id] = h
		}
	}
	return out, rows.Err()
}

// Vectors returns the cached matrix for a kind/model/dims, reloading it from
// SQLite after any write.
func (s *Store) Vectors(kind, modelID string, dims int) (*Matrix, error) {
	key := fmt.Sprintf("%s|%s|%d", kind, modelID, dims)
	s.vc.mu.Lock()
	gen := s.vc.gen
	if c, ok := s.vc.mats[key]; ok && c.gen == gen {
		s.vc.mu.Unlock()
		return c.m, nil
	}
	s.vc.mu.Unlock()

	rows, err := s.db.Query(`SELECT asset_id, vec FROM asset_vectors WHERE kind=? AND model=? AND dims=? ORDER BY asset_id`, kind, modelID, dims)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := &Matrix{Dims: dims, pos: map[int64]int{}}
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil || len(b) != 4*dims {
			continue
		}
		m.pos[id] = len(m.IDs)
		m.IDs = append(m.IDs, id)
		m.Vecs = appendVec(m.Vecs, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.vc.mu.Lock()
	if s.vc.mats == nil {
		s.vc.mats = map[string]*cachedMatrix{}
	}
	s.vc.mats[key] = &cachedMatrix{gen: gen, m: m}
	s.vc.mu.Unlock()
	return m, nil
}

// VectorCounts reports stored vectors per kind (any model).
func (s *Store) VectorCounts() map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM asset_vectors GROUP BY kind`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if rows.Scan(&k, &n) == nil {
			out[k] = n
		}
	}
	return out
}

// FilteredIDs returns the ids passing the query's facet filters, or nil when
// the query has no filters (everything passes).
func (s *Store) FilteredIDs(q model.Query) (map[int64]bool, error) {
	wheres, args := queryStrings(q, "")
	if len(wheres) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT id FROM assets WHERE `+strings.Join(wheres, " AND "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// GetByIDs loads assets, preserving the order of ids and skipping missing.
func (s *Store) GetByIDs(ids []int64) ([]model.Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT `+selectCols+` FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	byID := map[int64]model.Asset{}
	for _, a := range collect(rows) {
		byID[a.RowID] = a
	}
	out := make([]model.Asset, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// All returns every local asset (for embedding backfills).
func (s *Store) All() ([]model.Asset, error) {
	rows, err := s.db.Query(`SELECT ` + selectCols + ` FROM assets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return collect(rows), nil
}

// SetClassification fills category/style where the agent or importer left
// them empty; existing values are kept.
func (s *Store) SetClassification(id int64, category, style string) error {
	_, err := s.db.Exec(`UPDATE assets SET
		category = CASE WHEN category = '' THEN ? ELSE category END,
		style = CASE WHEN style = '' THEN ? ELSE style END,
		updated_at = ? WHERE id = ?`, category, style, now(), id)
	return err
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func appendVec(dst []float32, b []byte) []float32 {
	for i := 0; i+4 <= len(b); i += 4 {
		dst = append(dst, math.Float32frombits(binary.LittleEndian.Uint32(b[i:])))
	}
	return dst
}
