// Package semantic adds EmbeddingGemma 2 vector search to the local index:
// hybrid keyword + text-vector + preview-image-vector ranking, query by
// image, "find similar" across sources, and zero-shot category/style
// suggestions. Everything degrades to plain keyword search when no
// embedding server is configured or reachable.
package semantic

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"modon/internal/config"
	"modon/internal/embed"
	"modon/internal/model"
	"modon/internal/store"
)

// Ranking constants and cut-offs. A vector hit must stand out from the
// query's score distribution over the whole corpus. The largest of n
// unrelated scores already reaches z ~ sqrt(2 ln n), so the cut scales with
// library size: about 3.0 for 500 assets, 4.0 for 25k. Measured on a 24.5k
// library, gibberish queries keep 0-7 text hits at that cut while real
// queries keep tens to hundreds.
const (
	rrfK     = 60.0
	poolSize = 100 // candidates per signal before fusion
	// Below this many vectors the distribution says little (with n rows the
	// best possible z is sqrt(n-1)), so small libraries get no cut.
	minRowsForZ = 50
)

// zFloor is the z-score cut for a corpus of n vectors.
func zFloor(n int) float64 {
	return math.Max(2, math.Sqrt(2*math.Log(float64(n)))-0.5)
}

// usable reports whether a vector signal can be trusted: either it covers
// enough assets to judge, or the whole library is that small. A large
// library with only a handful of previews would otherwise return those few
// thumbnails for every query.
func usable(m, other *store.Matrix) bool {
	if m == nil || len(m.IDs) == 0 {
		return false
	}
	return len(m.IDs) >= minRowsForZ || len(other.IDs) < minRowsForZ
}

// neutralQueries define the "query about nothing in particular" whose
// similarity to each asset is subtracted from real query scores.
var neutralQueries = []string{"", "a 3D model", "an object", "a thing", "asset"}

// Engine owns the embedding client and the in-memory ranking state.
type Engine struct {
	cfg *config.Config
	st  *store.Store
	cl  *embed.Client

	mu        sync.Mutex
	neutral   []float32
	bias      map[*store.Matrix][]float32
	queries   map[string][]float32 // small cache of recent query vectors
	downUntil time.Time
	lastErr   string
	labels    *labelSet
	sidecar   *exec.Cmd
}

// New builds the engine; it is inert when embedding.endpoint is unset.
func New(cfg *config.Config, st *store.Store) *Engine {
	e := &Engine{cfg: cfg, st: st, bias: map[*store.Matrix][]float32{}, queries: map[string][]float32{}}
	ec := cfg.Embedding
	if ec.Endpoint != "" {
		e.cl = embed.New(ec.Endpoint, ec.Model, ec.Dims, time.Duration(ec.TimeoutSeconds)*time.Second)
	}
	return e
}

// Enabled reports whether an embedding endpoint is configured.
func (e *Engine) Enabled() bool { return e != nil && e.cl != nil }

func (e *Engine) modelID() string { return e.cl.Model }
func (e *Engine) dims() int       { return e.cl.Dims }

// available pings the server, remembering failures briefly so a down
// sidecar costs one probe per 30s rather than one per search.
func (e *Engine) available(ctx context.Context) bool {
	if !e.Enabled() {
		return false
	}
	e.mu.Lock()
	if time.Now().Before(e.downUntil) {
		e.mu.Unlock()
		return false
	}
	e.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := e.cl.Ping(pctx); err != nil {
		e.fail(err)
		e.mu.Lock()
		e.downUntil = time.Now().Add(30 * time.Second)
		e.mu.Unlock()
		return false
	}
	return true
}

func (e *Engine) fail(err error) {
	e.mu.Lock()
	e.lastErr = err.Error()
	e.mu.Unlock()
}

// Reachable reports whether the embedding server answers right now.
func (e *Engine) Reachable(ctx context.Context) bool { return e.available(ctx) }

// Start launches the bundled sidecar when embedding.autostart is set and
// nothing answers at the endpoint. It returns immediately when wait is 0;
// the sidecar keeps loading in the background.
func (e *Engine) Start(ctx context.Context, wait time.Duration) error {
	if !e.Enabled() || !e.cfg.Embedding.Autostart {
		return nil
	}
	cache, err := config.CacheDir()
	if err != nil {
		return err
	}
	ec := e.cfg.Embedding
	opts := embed.SidecarOptions{
		UV: ec.UVPath, CacheDir: cache, Endpoint: ec.Endpoint,
		ImageTokens: ec.ImageTokens, Device: ec.Device,
	}
	cmd, err := embed.Start(ctx, e.cl, opts, filepath.Join(cache, "embed_server.log"), wait)
	e.mu.Lock()
	if cmd != nil {
		e.sidecar = cmd
	}
	e.downUntil = time.Time{}
	e.mu.Unlock()
	return err
}

// Close stops a sidecar this engine started.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	cmd := e.sidecar
	e.sidecar = nil
	e.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
}

// ---- query embedding + scoring ----

func (e *Engine) queryVec(ctx context.Context, text string) ([]float32, error) {
	key := strings.ToLower(strings.TrimSpace(text))
	e.mu.Lock()
	if v, ok := e.queries[key]; ok {
		e.mu.Unlock()
		return v, nil
	}
	e.mu.Unlock()
	v, err := e.cl.EmbedOne(ctx, embed.Input{Text: embed.Query(text)})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if len(e.queries) > 256 {
		e.queries = map[string][]float32{}
	}
	e.queries[key] = v
	e.mu.Unlock()
	return v, nil
}

func (e *Engine) neutralVec(ctx context.Context) ([]float32, error) {
	e.mu.Lock()
	if e.neutral != nil {
		v := e.neutral
		e.mu.Unlock()
		return v, nil
	}
	e.mu.Unlock()
	ins := make([]embed.Input, len(neutralQueries))
	for i, q := range neutralQueries {
		ins[i] = embed.Input{Text: embed.Query(q)}
	}
	vs, err := e.cl.Embed(ctx, ins)
	if err != nil {
		return nil, err
	}
	mean := make([]float32, e.dims())
	for _, v := range vs {
		for i, x := range v {
			mean[i] += x
		}
	}
	mean, err = embed.Fit(mean, e.dims())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.neutral = mean
	e.mu.Unlock()
	return mean, nil
}

// baseline returns each row's similarity to the neutral query, cached per
// loaded matrix.
func (e *Engine) baseline(ctx context.Context, m *store.Matrix) ([]float32, error) {
	e.mu.Lock()
	if b, ok := e.bias[m]; ok {
		e.mu.Unlock()
		return b, nil
	}
	e.mu.Unlock()
	nv, err := e.neutralVec(ctx)
	if err != nil {
		return nil, err
	}
	b := make([]float32, len(m.IDs))
	for i := range m.IDs {
		b[i] = embed.Dot(nv, m.Vecs[i*m.Dims:(i+1)*m.Dims])
	}
	e.mu.Lock()
	if len(e.bias) > 8 { // matrices are replaced after writes; drop stale ones
		e.bias = map[*store.Matrix][]float32{}
	}
	e.bias[m] = b
	e.mu.Unlock()
	return b, nil
}

type hit struct {
	id    int64
	score float32
}

// rank scores every row against v and returns the top k. bias (may be nil)
// is subtracted per row. With minZ > 0, rows scoring less than minZ
// standard deviations above the corpus mean are dropped; the statistics use
// every row so facet filters do not shift the cut. Rows outside allowed
// (when non-nil) and the excluded id are skipped.
func rank(m *store.Matrix, v []float32, bias []float32, allowed map[int64]bool, exclude int64, minZ float64, k int) []hit {
	if m == nil || len(m.IDs) == 0 || len(v) != m.Dims {
		return nil
	}
	scores := make([]float32, len(m.IDs))
	var sum, sumSq float64
	for i := range m.IDs {
		s := embed.Dot(v, m.Vecs[i*m.Dims:(i+1)*m.Dims])
		if bias != nil {
			s -= bias[i]
		}
		scores[i] = s
		sum += float64(s)
		sumSq += float64(s) * float64(s)
	}
	floor := float32(math.Inf(-1))
	if minZ > 0 && len(scores) >= minRowsForZ {
		n := float64(len(scores))
		mean := sum / n
		sd := math.Sqrt(math.Max(sumSq/n-mean*mean, 0))
		floor = float32(mean + minZ*sd)
	}
	hits := make([]hit, 0, 256)
	for i, id := range m.IDs {
		if id == exclude || (allowed != nil && !allowed[id]) || scores[i] < floor {
			continue
		}
		hits = append(hits, hit{id, scores[i]})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

type signal struct {
	name   string
	weight float64
	ids    []int64
}

func hitIDs(h []hit) []int64 {
	out := make([]int64, len(h))
	for i, x := range h {
		out[i] = x.id
	}
	return out
}

// fuse merges ranked lists with reciprocal rank fusion and loads the top
// assets, annotating score and matched_by.
func (e *Engine) fuse(signals []signal, limit int) ([]model.Asset, error) {
	score := map[int64]float64{}
	by := map[int64][]string{}
	for _, s := range signals {
		for r, id := range s.ids {
			score[id] += s.weight / (rrfK + float64(r+1))
			by[id] = append(by[id], s.name)
		}
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	assets, err := e.st.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	// Normalize so a hit ranked first by every signal scores 1.0.
	var maxScore float64
	for _, s := range signals {
		if len(s.ids) > 0 {
			maxScore += s.weight / (rrfK + 1)
		}
	}
	for i := range assets {
		id := assets[i].RowID
		if maxScore > 0 {
			assets[i].Score = math.Round(score[id]/maxScore*1000) / 1000
		}
		assets[i].MatchedBy = by[id]
	}
	return assets, nil
}

// zpart is one vector signal for score fusion.
type zpart struct {
	name   string
	weight float64
	m      *store.Matrix
	v      []float32
}

// fuseZ combines vector signals by weighted z-score: each signal's cosine
// is standardized over its whole matrix, so a near-identical match (z far
// above 3) dominates instead of being flattened the way rank fusion would.
// Assets lacking a signal (e.g. no preview) get no contribution from it.
// Used when every signal is a vector score; keyword search uses fuse (RRF).
func (e *Engine) fuseZ(parts []zpart, allowed map[int64]bool, exclude int64, limit int) ([]model.Asset, error) {
	score := map[int64]float64{}
	by := map[int64][]string{}
	for _, p := range parts {
		m := p.m
		if m == nil || len(m.IDs) == 0 || len(p.v) != m.Dims || !usable(m, largest(parts)) {
			continue
		}
		raw := make([]float64, len(m.IDs))
		var sum, sumSq float64
		for i := range m.IDs {
			x := float64(embed.Dot(p.v, m.Vecs[i*m.Dims:(i+1)*m.Dims]))
			raw[i] = x
			sum += x
			sumSq += x * x
		}
		n := float64(len(raw))
		mean := sum / n
		sd := math.Sqrt(math.Max(sumSq/n-mean*mean, 1e-12))
		for i, id := range m.IDs {
			if id == exclude || (allowed != nil && !allowed[id]) {
				continue
			}
			z := (raw[i] - mean) / sd
			score[id] += p.weight * z
			if z >= 2 {
				by[id] = append(by[id], p.name)
			}
		}
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	assets, err := e.st.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	for i := range assets {
		assets[i].Score = math.Round(score[assets[i].RowID]*100) / 100
		assets[i].MatchedBy = by[assets[i].RowID]
	}
	return assets, nil
}

func largest(parts []zpart) *store.Matrix {
	var big *store.Matrix
	for _, p := range parts {
		if p.m != nil && (big == nil || len(p.m.IDs) > len(big.IDs)) {
			big = p.m
		}
	}
	return big
}

func (e *Engine) matrices() (text, image *store.Matrix, err error) {
	if text, err = e.st.Vectors(store.VecText, e.modelID(), e.dims()); err != nil {
		return nil, nil, err
	}
	if image, err = e.st.Vectors(store.VecImage, e.modelID(), e.dims()); err != nil {
		return nil, nil, err
	}
	return text, image, nil
}

// ---- search ----

// Search runs hybrid local search. Without a reachable embedding server it
// is exactly the keyword search.
func (e *Engine) Search(ctx context.Context, q model.Query) ([]model.Asset, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 12
	}
	if (q.Text == "" && q.Image == "") || !e.available(ctx) {
		if q.Image != "" && q.Text == "" {
			return nil, fmt.Errorf("image search needs the embedding server (%s)", e.statusHint())
		}
		return e.st.Search(q)
	}

	var signals []signal
	if q.Text != "" {
		kq := q
		kq.Limit = poolSize
		kw, mode, err := e.st.SearchMode(kq)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, len(kw))
		for i, a := range kw {
			ids[i] = a.RowID
		}
		// Loose keyword fallbacks (any token, substring) are mostly noise
		// for multi-word queries; the vectors carry those.
		w := 1.0
		if mode != store.MatchAll {
			w = 0.35
		}
		signals = append(signals, signal{"keyword", w, ids})
	}

	allowed, err := e.st.FilteredIDs(q)
	if err != nil {
		return nil, err
	}
	textM, imageM, err := e.matrices()
	if err != nil {
		return nil, err
	}

	if q.Text != "" {
		qv, err := e.queryVec(ctx, q.Text)
		if err != nil {
			e.fail(err)
			return e.st.Search(q)
		}
		// Text documents are scored raw; preview images are corrected by
		// their similarity to a neutral query, because cross-modal scores
		// carry a large per-image offset (every thumbnail is "a 3D model").
		ib, err := e.baseline(ctx, imageM)
		if err != nil {
			return e.st.Search(q)
		}
		if usable(textM, imageM) {
			signals = append(signals, signal{"text", 1, hitIDs(rank(textM, qv, nil, allowed, 0, zFloor(len(textM.IDs)), poolSize))})
		}
		if usable(imageM, textM) {
			signals = append(signals, signal{"image", 1, hitIDs(rank(imageM, qv, ib, allowed, 0, zFloor(len(imageM.IDs)), poolSize))})
		}
	}
	if q.Image != "" {
		iv, err := e.cl.EmbedOne(ctx, embed.Input{Image: q.Image})
		if err != nil {
			return nil, fmt.Errorf("embed query image: %w", err)
		}
		if q.Text == "" {
			// Pure picture query: visual similarity leads; the picture vs
			// text documents reaches assets that have no preview.
			return e.fuseZ([]zpart{{"image", 1, imageM, iv}, {"text", 0.3, textM, iv}}, allowed, 0, limit)
		}
		signals = append(signals,
			signal{"image", 1.5, hitIDs(rank(imageM, iv, nil, allowed, 0, 0, poolSize))},
			signal{"text", 0.5, hitIDs(rank(textM, iv, nil, allowed, 0, 0, poolSize))})
	}
	return e.fuse(signals, limit)
}

// ---- similar ----

// Ref identifies the asset to find look-alikes for: a local asset (RowID
// set) or a remote one with its preview bytes.
type Ref struct {
	Asset     model.Asset
	Image     []byte
	ImageMIME string
}

// Similar returns local assets that look like (image) or are described like
// (text) the reference. Facet filters in q apply; q.Text is ignored.
func (e *Engine) Similar(ctx context.Context, ref Ref, q model.Query) ([]model.Asset, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("find_similar needs the embedding server (%s)", e.statusHint())
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 12
	}
	allowed, err := e.st.FilteredIDs(q)
	if err != nil {
		return nil, err
	}
	textM, imageM, err := e.matrices()
	if err != nil {
		return nil, err
	}

	var iv, tv []float32
	self := ref.Asset.RowID
	if self != 0 {
		iv = imageM.Row(self)
		tv = textM.Row(self)
	}
	if iv == nil && len(ref.Image) > 0 && e.vision(ctx) {
		if iv, err = e.cl.EmbedOne(ctx, embed.Input{Image: embed.DataURL(ref.Image, ref.ImageMIME)}); err != nil {
			return nil, fmt.Errorf("embed preview: %w", err)
		}
	}
	if tv == nil {
		title, body := DocText(ref.Asset)
		if tv, err = e.cl.EmbedOne(ctx, embed.Input{Text: embed.Document(title, body)}); err != nil {
			return nil, err
		}
	}

	parts := []zpart{{"text", 1, textM, tv}}
	if iv != nil {
		parts = []zpart{{"image", 1, imageM, iv}, {"text", 1, textM, tv}}
	}
	out, err := e.fuseZ(parts, allowed, self, limit)
	if err != nil {
		return nil, err
	}
	// Raw visual similarity is what tells an agent "you own something that
	// looks like this" before buying.
	if iv != nil {
		for i := range out {
			if row := imageM.Row(out[i].RowID); row != nil {
				out[i].Similarity = math.Round(float64(embed.Dot(iv, row))*1000) / 1000
			}
		}
	}
	return out, nil
}

func (e *Engine) vision(ctx context.Context) bool {
	info, err := e.cl.Info(ctx)
	return err == nil && info.Vision
}

// ---- status ----

func (e *Engine) statusHint() string {
	if !e.Enabled() {
		return "set embedding.endpoint, e.g. `modon config set embedding.endpoint http://127.0.0.1:8178` and run `modon embed-server`"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastErr != "" {
		return "server at " + e.cl.Endpoint + " unavailable: " + e.lastErr
	}
	return "server at " + e.cl.Endpoint + " unavailable"
}

// Status summarizes the semantic layer for index_stats.
func (e *Engine) Status(ctx context.Context) map[string]any {
	out := map[string]any{"enabled": e.Enabled()}
	if !e.Enabled() {
		out["hint"] = e.statusHint()
		return out
	}
	out["endpoint"] = e.cl.Endpoint
	out["model"] = e.cl.Model
	out["dims"] = e.cl.Dims
	out["vectors"] = e.st.VectorCounts()
	if e.available(ctx) {
		info, _ := e.cl.Info(ctx)
		out["server"] = info
		out["reachable"] = true
	} else {
		out["reachable"] = false
		out["hint"] = e.statusHint()
	}
	return out
}
