package semantic

import (
	"modon/internal/embed"
	"modon/internal/naming"
	"modon/internal/store"
)

// DupImageCosine is the preview similarity above which two same-named
// assets from different packs count as one model. Measured on a 24k-asset
// library: random preview pairs reach 0.947 only at the 99.9th percentile,
// while a prop shared between packs scores 0.97-1.0; same-named pieces
// that merely share a generic name ("Wall 01") mostly fall below 0.95.
const DupImageCosine = 0.965

// DedupeStats summarizes a clustering run.
type DedupeStats struct {
	Assets   int `json:"assets"`
	Clusters int `json:"clusters"`         // clusters with 2+ members
	Copies   int `json:"redundant_copies"` // members beyond the first per cluster
	Changed  int `json:"rows_changed"`
}

// Dedupe recomputes duplicate clusters for the whole index. Two assets are
// copies of one model when they have the same normalized name and either
// come from the same pack (the same model exported as FBX and OBJ, whose
// previews can differ because one format lost its textures) or have
// near-identical preview vectors (a prop shared between packs). Clusters
// are transitive; the cluster id is the smallest member id.
func (e *Engine) Dedupe() (*DedupeStats, error) {
	var img *store.Matrix
	if e.Enabled() {
		var err error
		if img, err = e.st.Vectors(store.VecImage, e.modelID(), e.dims()); err != nil {
			return nil, err
		}
	}
	return Dedupe(e.st, img)
}

// Dedupe clusters st using preview vectors from img (nil = names and packs
// only).
func Dedupe(st *store.Store, img *store.Matrix) (*DedupeStats, error) {
	in, err := st.ClusterInputs()
	if err != nil {
		return nil, err
	}
	parent := map[int64]int64{}
	var find func(int64) int64
	find = func(x int64) int64 {
		p, ok := parent[x]
		if !ok || p == x {
			return x
		}
		r := find(p)
		parent[x] = r
		return r
	}
	union := func(a, b int64) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	type member struct {
		id   int64
		pack string
	}
	groups := map[string][]member{}
	for _, c := range in {
		if k := naming.DupName(c.Title); k != "" {
			groups[k] = append(groups[k], member{c.ID, naming.PackKey(c.Path)})
		}
	}
	for _, g := range groups {
		for i := 0; i < len(g); i++ {
			for j := i + 1; j < len(g); j++ {
				a, b := g[i], g[j]
				if a.pack == b.pack {
					union(a.id, b.id)
					continue
				}
				va, vb := img.Row(a.id), img.Row(b.id)
				if va != nil && vb != nil && float64(embed.Dot(va, vb)) >= DupImageCosine {
					union(a.id, b.id)
				}
			}
		}
	}
	size := map[int64]int{}
	for _, c := range in {
		size[find(c.ID)]++
	}
	cluster := make(map[int64]int64, len(in))
	ds := &DedupeStats{Assets: len(in)}
	for _, c := range in {
		r := find(c.ID)
		if size[r] > 1 {
			cluster[c.ID] = r
		}
	}
	for _, n := range size {
		if n > 1 {
			ds.Clusters++
			ds.Copies += n - 1
		}
	}
	if ds.Changed, err = st.SetClusters(cluster); err != nil {
		return nil, err
	}
	return ds, nil
}
