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
	"strconv"
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
// library (text documents and preview images alike), gibberish queries keep
// 0-7 hits per signal at that cut while real queries keep their matches.
// Raw cosines separated better than baseline-corrected ones at that scale.
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

// Engine owns the embedding client and the in-memory ranking state.
type Engine struct {
	cfg *config.Config
	st  *store.Store
	cl  *embed.Client

	mu        sync.Mutex
	queries   map[string][]float32 // small cache of recent query vectors
	downUntil time.Time
	lastErr   string
	labels    *labelSet
	sidecar   *exec.Cmd
}

// New builds the engine; it is inert when embedding.endpoint is unset.
func New(cfg *config.Config, st *store.Store) *Engine {
	e := &Engine{cfg: cfg, st: st, queries: map[string][]float32{}}
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

type hit struct {
	id    int64
	score float32
}

// rank scores every row against v and returns the top k. With minZ > 0, rows scoring less than minZ
// standard deviations above the corpus mean are dropped; the statistics use
// every row so facet filters do not shift the cut. Rows outside allowed
// (when non-nil) and the excluded id are skipped.
func rank(m *store.Matrix, v []float32, allowed map[int64]bool, exclude int64, minZ float64, k int) []hit {
	if m == nil || len(m.IDs) == 0 || len(v) != m.Dims {
		return nil
	}
	scores := make([]float32, len(m.IDs))
	var sum, sumSq float64
	for i := range m.IDs {
		s := embed.Dot(v, m.Vecs[i*m.Dims:(i+1)*m.Dims])
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
	if q.StyleRef != "" && !e.available(ctx) {
		return nil, fmt.Errorf("style_reference needs the embedding server (%s)", e.statusHint())
	}
	if (q.Text == "" && q.Image == "" && q.StyleRef == "") || !e.available(ctx) {
		if q.Image != "" && q.Text == "" {
			return nil, fmt.Errorf("image search needs the embedding server (%s)", e.statusHint())
		}
		return e.st.Search(q)
	}

	var sv []float32 // style reference vector
	if q.StyleRef != "" {
		_, imageM, err := e.matrices()
		if err != nil {
			return nil, err
		}
		if sv, err = e.styleVec(ctx, q.StyleRef, imageM); err != nil {
			return nil, err
		}
		if q.Text == "" && q.Image == "" {
			// Nothing to be relevant to: "things that look like this".
			allowed, err := e.st.FilteredIDs(q)
			if err != nil {
				return nil, err
			}
			out, err := e.fuseZ([]zpart{{"style", 1, imageM, sv}}, allowed, 0, limit)
			setSimilarity(out, sv, imageM)
			return out, err
		}
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
		if usable(textM, imageM) {
			signals = append(signals, signal{"text", 1, hitIDs(rank(textM, qv, allowed, 0, zFloor(len(textM.IDs)), poolSize))})
		}
		if usable(imageM, textM) {
			signals = append(signals, signal{"image", 1, hitIDs(rank(imageM, qv, allowed, 0, zFloor(len(imageM.IDs)), poolSize))})
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
			signal{"image", 1.5, hitIDs(rank(imageM, iv, allowed, 0, 0, poolSize))},
			signal{"text", 0.5, hitIDs(rank(textM, iv, allowed, 0, 0, poolSize))})
	}
	if sv == nil {
		return e.fuse(signals, limit)
	}
	pool := max(3*limit, 30)
	cands, err := e.fuse(signals, pool)
	if err != nil {
		return nil, err
	}
	return restyle(cands, sv, imageM, q.StyleWeight, limit), nil
}

// styleVec resolves a style reference: "local:<id>" uses that asset's
// stored preview vector, anything else is embedded as an image.
func (e *Engine) styleVec(ctx context.Context, ref string, imageM *store.Matrix) ([]float32, error) {
	if rest, ok := strings.CutPrefix(ref, model.SourceLocal+":"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("style reference %q: local ids are numeric", ref)
		}
		if v := imageM.Row(id); v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("style reference %s has no preview vector yet (modon embed --render)", ref)
	}
	v, err := e.cl.EmbedOne(ctx, embed.Input{Image: ref})
	if err != nil {
		return nil, fmt.Errorf("embed style reference: %w", err)
	}
	return v, nil
}

// restyle re-ranks relevance-ordered candidates toward a style reference.
// The query text already decided *what* the candidates are (all barrels),
// so among them preview similarity mostly reflects the look: low-poly vs
// realistic, palette, texture treatment. Final order blends relevance rank
// and style rank; weight is the style share (default 0.5). Candidates
// without a preview sit just below the middle on style. Similarity reports
// each hit's preview cosine to the reference.
func restyle(cands []model.Asset, sv []float32, imageM *store.Matrix, weight float64, limit int) []model.Asset {
	if weight <= 0 || weight > 1 {
		weight = 0.5
	}
	n := len(cands)
	if n == 0 {
		return cands
	}
	setSimilarity(cands, sv, imageM)
	withImg := make([]int, 0, n)
	for i, a := range cands {
		if imageM.Row(a.RowID) != nil {
			withImg = append(withImg, i)
		}
	}
	sort.SliceStable(withImg, func(x, y int) bool { return cands[withImg[x]].Similarity > cands[withImg[y]].Similarity })
	styleNorm := make([]float64, n)
	for i := range styleNorm {
		styleNorm[i] = 0.4
	}
	for pos, i := range withImg {
		styleNorm[i] = 1 - float64(pos)/float64(len(withImg))
		if pos < (len(withImg)+2)/3 {
			cands[i].MatchedBy = append(cands[i].MatchedBy, "style")
		}
	}
	blend := make([]float64, n)
	for i := range cands {
		blend[i] = (1-weight)*(1-float64(i)/float64(n)) + weight*styleNorm[i]
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool { return blend[idx[x]] > blend[idx[y]] })
	out := make([]model.Asset, 0, min(limit, n))
	for _, i := range idx[:min(limit, n)] {
		a := cands[i]
		a.Score = math.Round(blend[i]*1000) / 1000
		out = append(out, a)
	}
	return out
}

// setSimilarity fills Similarity with each asset's preview cosine to v.
func setSimilarity(assets []model.Asset, v []float32, imageM *store.Matrix) {
	for i := range assets {
		if row := imageM.Row(assets[i].RowID); row != nil {
			assets[i].Similarity = math.Round(float64(embed.Dot(v, row))*1000) / 1000
		}
	}
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
		setSimilarity(out, iv, imageM)
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
