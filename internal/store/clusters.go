package store

import (
	"context"
	"fmt"

	"modon/internal/model"
)

// ClusterInput is the per-asset data duplicate detection needs.
type ClusterInput struct {
	ID    int64
	Title string
	Path  string
}

// ClusterInputs returns id, title and path for every asset.
func (s *Store) ClusterInputs() ([]ClusterInput, error) {
	rows, err := s.db.Query(`SELECT id, title, path FROM assets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClusterInput
	for rows.Next() {
		var c ClusterInput
		if err := rows.Scan(&c.ID, &c.Title, &c.Path); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetClusters stores cluster ids (asset id -> cluster id, 0 = no
// duplicates) for every asset in one transaction, writing only rows whose
// cluster changed. It returns the number of rows updated.
func (s *Store) SetClusters(cluster map[int64]int64) (int, error) {
	cur := map[int64]int64{}
	rows, err := s.db.Query(`SELECT id, cluster_id FROM assets`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id, c int64
		if err := rows.Scan(&id, &c); err != nil {
			rows.Close()
			return 0, err
		}
		cur[id] = c
	}
	rows.Close()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := tx.Prepare(`UPDATE assets SET cluster_id = ? WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	n := 0
	for id, old := range cur {
		if c := cluster[id]; c != old {
			if _, err := st.Exec(c, id); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, tx.Commit()
}

// ClusterStats reports how many clusters exist and how many assets are
// redundant copies (members beyond the first of each cluster).
func (s *Store) ClusterStats() (clusters, copies int) {
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT cluster_id), COUNT(*) - COUNT(DISTINCT cluster_id)
		FROM assets WHERE cluster_id != 0`).Scan(&clusters, &copies)
	return clusters, copies
}

// ClusterMembers returns every asset id in a cluster, ascending.
func (s *Store) ClusterMembers(cluster int64) ([]int64, error) {
	if cluster == 0 {
		return nil, nil
	}
	return clusterMembers(context.Background(), s.db, cluster, false)
}

// CollapseDuplicates keeps the first (best-ranked) asset of each duplicate
// cluster, records the cluster's other members in its Duplicates, and cuts
// the list to limit.
func (s *Store) CollapseDuplicates(assets []model.Asset, limit int) ([]model.Asset, error) {
	seen := map[int64]bool{}
	out := make([]model.Asset, 0, min(len(assets), max(limit, 0)))
	for _, a := range assets {
		if limit > 0 && len(out) >= limit {
			break
		}
		if a.ClusterID != 0 {
			if seen[a.ClusterID] {
				continue
			}
			seen[a.ClusterID] = true
		}
		out = append(out, a)
	}
	for i := range out {
		if out[i].ClusterID == 0 {
			continue
		}
		ids, err := s.ClusterMembers(out[i].ClusterID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if id != out[i].RowID {
				out[i].Duplicates = append(out[i].Duplicates, fmt.Sprintf("local:%d", id))
			}
		}
	}
	return out, nil
}
