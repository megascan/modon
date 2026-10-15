package semantic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"modon/internal/classify"
	"modon/internal/embed"
	"modon/internal/model"
	"modon/internal/store"
)

// styleLabels are the visually recognisable art styles, phrased for
// zero-shot matching against preview images.
var styleLabels = map[string]string{
	"lowpoly":     "a low-poly 3D model with flat shaded faceted geometry",
	"stylized":    "a stylized 3D model with exaggerated simplified shapes",
	"realistic":   "a photorealistic 3D model with detailed realistic textures",
	"voxel":       "a voxel 3D model built from little cubes",
	"cartoon":     "a cute cartoon 3D model with bright saturated colors",
	"scifi":       "a sci-fi 3D model, futuristic technology",
	"fantasy":     "a fantasy 3D model, magical and medieval-fantasy",
	"medieval":    "a medieval 3D model",
	"modern":      "a modern contemporary everyday 3D model",
	"handpainted": "a 3D model with hand-painted textures",
	"ancient":     "an ancient Roman, Greek or Egyptian 3D model",
	"asian":       "an East Asian, Japanese or Chinese themed 3D model",
	"steampunk":   "a steampunk 3D model with brass gears and pipes",
	"cyberpunk":   "a cyberpunk neon 3D model",
	"retro":       "a retro vintage 3D model",
}

type label struct {
	name string
	vec  []float32
}

type labelSet struct {
	categories []label
	styles     []label
	tags       []label // vocabulary terms
	tagFacet   []string
}

func categoryPrompt(cat string, kws []string) string {
	var words []string
	for _, k := range kws {
		if k != cat && len(words) < 6 {
			words = append(words, k)
		}
	}
	if len(words) == 0 {
		return "a 3D model of a " + cat
	}
	return "a 3D model of a " + cat + ", such as a " + strings.Join(words, ", ")
}

func (e *Engine) loadLabels(ctx context.Context) (*labelSet, error) {
	e.mu.Lock()
	if e.labels != nil {
		ls := e.labels
		e.mu.Unlock()
		return ls, nil
	}
	e.mu.Unlock()

	vocab := classify.CategoryVocabulary()
	var cats, styles []string
	for c := range vocab {
		cats = append(cats, c)
	}
	for s := range styleLabels {
		styles = append(styles, s)
	}
	sort.Strings(cats)
	sort.Strings(styles)

	ins := make([]embed.Input, 0, len(cats)+len(styles))
	for _, c := range cats {
		ins = append(ins, embed.Input{Text: embed.Query(categoryPrompt(c, vocab[c]))})
	}
	for _, s := range styles {
		ins = append(ins, embed.Input{Text: embed.Query(styleLabels[s])})
	}
	var terms, facets []string
	for _, f := range classify.TagVocabulary {
		for _, t := range f.Terms {
			terms = append(terms, t)
			facets = append(facets, f.Name)
			ins = append(ins, embed.Input{Text: embed.Query(fmt.Sprintf(f.Prompt, strings.ReplaceAll(t, "-", " ")))})
		}
	}
	vecs, err := e.cl.Embed(ctx, ins)
	if err != nil {
		return nil, err
	}
	ls := &labelSet{tagFacet: facets}
	for i, c := range cats {
		ls.categories = append(ls.categories, label{c, vecs[i]})
	}
	for i, s := range styles {
		ls.styles = append(ls.styles, label{s, vecs[len(cats)+i]})
	}
	for i, t := range terms {
		ls.tags = append(ls.tags, label{t, vecs[len(cats)+len(styles)+i]})
	}
	e.mu.Lock()
	e.labels = ls
	e.mu.Unlock()
	return ls, nil
}

// centroid is the mean row of a matrix; subtracting a label's similarity to
// it removes labels that are close to every asset.
func centroid(m *store.Matrix) []float32 {
	if m == nil || len(m.IDs) == 0 {
		return nil
	}
	c := make([]float32, m.Dims)
	for i := range m.IDs {
		row := m.Vecs[i*m.Dims : (i+1)*m.Dims]
		for j, x := range row {
			c[j] += x
		}
	}
	inv := 1 / float32(len(m.IDs))
	for j := range c {
		c[j] *= inv
	}
	return c
}

// Suggestion is one candidate label with its score: for zero-shot labels
// the centered similarity, for neighbour votes the similarity-weighted
// share of labelled look-alikes carrying it.
type Suggestion struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
	Facet string  `json:"facet,omitempty"` // vocabulary facet of a tag
	Count int     `json:"count,omitempty"` // neighbours carrying it
}

// Neighbor is a labelled look-alike that voted in a suggestion.
type Neighbor struct {
	ID         string  `json:"id"`
	Similarity float64 `json:"similarity"`
}

// Suggestions are the candidates for one asset. Categories, Styles and
// Tags (the recommended vocabulary, best per facet) are zero-shot matches
// of the preview image and text document; NeighborTags and
// NeighborCategories are what agents wrote on the most similar
// agent-tagged assets (Neighbors). Margin is the gap between the first two
// zero-shot categories; Confident marks margins above the threshold
// auto-classification uses.
type Suggestions struct {
	ID                 string       `json:"id"`
	Title              string       `json:"title"`
	Current            [2]string    `json:"current"` // category, style
	Categories         []Suggestion `json:"categories"`
	Styles             []Suggestion `json:"styles"`
	Tags               []Suggestion `json:"tags,omitempty"`
	NeighborCategories []Suggestion `json:"neighbor_categories,omitempty"`
	NeighborTags       []Suggestion `json:"neighbor_tags,omitempty"`
	Neighbors          []Neighbor   `json:"neighbors,omitempty"`
	Basis              []string     `json:"basis"` // image and/or text
	Margin             float64      `json:"margin"`
	Confident          bool         `json:"confident"`
	Error              string       `json:"error,omitempty"`
}

// ConfidentMargin is the category score gap above which auto-classification
// fills an empty category (calibrated with `modon embed --eval`).
const ConfidentMargin = 0.05

type scorer struct {
	ls         *labelSet
	img, txt   *store.Matrix
	cImg, cTxt []float32
	useText    bool
	labelled   map[int64]store.AgentMeta // neighbour examples (nil: none loaded)
}

func (e *Engine) newScorer(ctx context.Context, useText bool) (*scorer, error) {
	ls, err := e.loadLabels(ctx)
	if err != nil {
		return nil, err
	}
	txt, img, err := e.matrices()
	if err != nil {
		return nil, err
	}
	return &scorer{ls: ls, img: img, txt: txt, cImg: centroid(img), cTxt: centroid(txt), useText: useText}, nil
}

func scoreLabels(labels []label, sources [][2][]float32) []Suggestion {
	out := make([]Suggestion, len(labels))
	for i, l := range labels {
		var s float32
		for _, src := range sources {
			v, c := src[0], src[1]
			s += embed.Dot(l.vec, v)
			if c != nil {
				s -= embed.Dot(l.vec, c)
			}
		}
		out[i] = Suggestion{Label: l.name, Score: float64(s) / float64(len(sources))}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func (sc *scorer) sources(id int64) (sources [][2][]float32, basis []string) {
	if v := sc.img.Row(id); v != nil {
		sources = append(sources, [2][]float32{v, sc.cImg})
		basis = append(basis, "image")
	}
	if sc.useText {
		if v := sc.txt.Row(id); v != nil {
			sources = append(sources, [2][]float32{v, sc.cTxt})
			basis = append(basis, "text")
		}
	}
	return sources, basis
}

func (sc *scorer) suggest(id int64) (cats, styles []Suggestion, basis []string) {
	sources, basis := sc.sources(id)
	if len(sources) == 0 {
		return nil, nil, nil
	}
	return scoreLabels(sc.ls.categories, sources), scoreLabels(sc.ls.styles, sources), basis
}

// vocabularyTags scores the tag vocabulary and keeps the best perFacet
// terms of each facet, best first overall.
func (sc *scorer) vocabularyTags(id int64, perFacet int) []Suggestion {
	sources, _ := sc.sources(id)
	if len(sources) == 0 || len(sc.ls.tags) == 0 {
		return nil
	}
	facetOf := map[string]string{}
	for i, l := range sc.ls.tags {
		facetOf[l.name] = sc.ls.tagFacet[i]
	}
	taken := map[string]int{}
	var out []Suggestion
	for _, s := range scoreLabels(sc.ls.tags, sources) {
		f := facetOf[s.Label]
		if taken[f] >= perFacet {
			continue
		}
		taken[f]++
		out = append(out, Suggestion{Label: s.Label, Score: round3(s.Score), Facet: f})
	}
	return out
}

// neighbourCount is how many labelled look-alikes vote.
const neighbourCount = 8

// neighbours votes with the agent-written tags and categories of the most
// similar agent-tagged assets (preview and text similarity averaged where
// both exist). Tag scores are similarity-weighted shares in 0..1.
func (sc *scorer) neighbours(id int64, k int) (cats, tags []Suggestion, near []Neighbor) {
	if len(sc.labelled) == 0 {
		return nil, nil, nil
	}
	iv, tv := sc.img.Row(id), sc.txt.Row(id)
	if iv == nil && tv == nil {
		return nil, nil, nil
	}
	type cand struct {
		id  int64
		sim float64
	}
	var cs []cand
	for lid := range sc.labelled {
		if lid == id {
			continue
		}
		var sum float64
		var n int
		if iv != nil {
			if r := sc.img.Row(lid); r != nil {
				sum += float64(embed.Dot(iv, r))
				n++
			}
		}
		if tv != nil {
			if r := sc.txt.Row(lid); r != nil {
				sum += float64(embed.Dot(tv, r))
				n++
			}
		}
		if n > 0 {
			cs = append(cs, cand{lid, sum / float64(n)})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].sim != cs[j].sim {
			return cs[i].sim > cs[j].sim
		}
		return cs[i].id < cs[j].id
	})
	if len(cs) > neighbourCount {
		cs = cs[:neighbourCount]
	}
	var total float64
	tagW, catW := map[string]float64{}, map[string]float64{}
	tagN, catN := map[string]int{}, map[string]int{}
	for _, c := range cs {
		w := math.Max(c.sim, 0)
		total += w
		m := sc.labelled[c.id]
		seen := map[string]bool{}
		for _, t := range m.Tags {
			t = strings.ToLower(t)
			if boilerTags[t] || seen[t] {
				continue
			}
			seen[t] = true
			tagW[t] += w
			tagN[t]++
		}
		if m.Category != "" {
			catW[m.Category] += w
			catN[m.Category]++
		}
		near = append(near, Neighbor{ID: fmt.Sprintf("local:%d", c.id), Similarity: round3(c.sim)})
	}
	if total == 0 {
		return nil, nil, near
	}
	vote := func(w map[string]float64, n map[string]int, limit int) []Suggestion {
		out := make([]Suggestion, 0, len(w))
		for l, x := range w {
			out = append(out, Suggestion{Label: l, Score: round3(x / total), Count: n[l]})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Score != out[j].Score {
				return out[i].Score > out[j].Score
			}
			return out[i].Label < out[j].Label
		})
		if len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	return vote(catW, catN, 3), vote(tagW, tagN, k), near
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func top(s []Suggestion, n int) []Suggestion {
	if len(s) > n {
		s = s[:n]
	}
	out := make([]Suggestion, len(s))
	for i, x := range s {
		out[i] = Suggestion{Label: x.Label, Score: round3(x.Score)}
	}
	return out
}

// Suggest returns ranked category/style/tag candidates for a local asset
// from its preview image and text document, plus what agents wrote on its
// labelled look-alikes. k caps each list (default 3; neighbour tags get
// 4k).
func (e *Engine) Suggest(ctx context.Context, id int64, k int) (*Suggestions, error) {
	out, err := e.SuggestMany(ctx, []int64{id}, k)
	if err != nil {
		return nil, err
	}
	if out[0].Error != "" {
		return nil, errors.New(out[0].Error)
	}
	return out[0], nil
}

// SuggestMany is Suggest for a batch: changed assets are embedded in one
// pass and the labels and labelled examples are loaded once. Assets that
// cannot be scored carry Error instead of failing the batch.
func (e *Engine) SuggestMany(ctx context.Context, ids []int64, k int) ([]*Suggestions, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("suggestions need the embedding server (%s)", e.statusHint())
	}
	if k <= 0 {
		k = 3
	}
	e.Refresh(ctx, ids...) // incremental: embeds only what changed
	assets, err := e.st.GetByIDs(ids)
	if err != nil {
		return nil, err
	}
	byID := map[int64]model.Asset{}
	for _, a := range assets {
		byID[a.RowID] = a
	}
	sc, err := e.newScorer(ctx, true)
	if err != nil {
		return nil, err
	}
	if sc.labelled, err = e.st.AgentTagged(); err != nil {
		return nil, err
	}
	out := make([]*Suggestions, 0, len(ids))
	for _, id := range ids {
		a, ok := byID[id]
		if !ok {
			out = append(out, &Suggestions{ID: fmt.Sprintf("local:%d", id), Error: fmt.Sprintf("no local asset with id %d", id)})
			continue
		}
		sg := &Suggestions{ID: a.ID, Title: a.Title, Current: [2]string{a.Category, a.Style}}
		cats, styles, basis := sc.suggest(id)
		if cats == nil {
			sg.Error = fmt.Sprintf("%s has no vectors yet; run `modon embed` (add --render for previews)", a.ID)
			out = append(out, sg)
			continue
		}
		margin := cats[0].Score - cats[1].Score
		sg.Categories, sg.Styles, sg.Basis = top(cats, k), top(styles, k), basis
		sg.Margin, sg.Confident = round3(margin), margin >= ConfidentMargin
		sg.Tags = sc.vocabularyTags(id, max(1, k/2))
		sg.NeighborCategories, sg.NeighborTags, sg.Neighbors = sc.neighbours(id, 4*k)
		out = append(out, sg)
	}
	return out, nil
}

// ClassifyStats summarizes an auto-classification run.
type ClassifyStats struct {
	Considered int            `json:"considered"`
	Filled     int            `json:"filled"`
	LowMargin  int            `json:"skipped_low_margin"`
	ByCategory map[string]int `json:"filled_by_category"`
	Applied    bool           `json:"applied"`
}

// AutoClassify fills empty categories (and empty styles) with confident
// zero-shot guesses. Existing values are never overwritten, and needs_review
// stays set so agents still write real descriptions.
func (e *Engine) AutoClassify(ctx context.Context, apply bool, margin float64) (*ClassifyStats, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("embedding server unavailable: %s", e.statusHint())
	}
	if margin <= 0 {
		margin = ConfidentMargin
	}
	sc, err := e.newScorer(ctx, true)
	if err != nil {
		return nil, err
	}
	assets, err := e.st.All()
	if err != nil {
		return nil, err
	}
	st := &ClassifyStats{ByCategory: map[string]int{}, Applied: apply}
	for _, a := range assets {
		if a.Category != "" || ctx.Err() != nil {
			continue
		}
		cats, styles, _ := sc.suggest(a.RowID)
		if cats == nil {
			continue
		}
		st.Considered++
		if cats[0].Score-cats[1].Score < margin {
			st.LowMargin++
			continue
		}
		style := ""
		if a.Style == "" && styles[0].Score-styles[1].Score >= margin {
			style = styles[0].Label
		}
		st.Filled++
		st.ByCategory[cats[0].Label]++
		if apply {
			if err := e.st.SetClassification(a.RowID, cats[0].Label, style); err != nil {
				return st, err
			}
		}
	}
	return st, ctx.Err()
}

// EvalResult reports zero-shot category accuracy against the heuristic
// labels of already-categorized assets, by margin bucket.
type EvalResult struct {
	Assets   int                `json:"assets_with_image_and_category"`
	Top1     float64            `json:"top1_accuracy"`
	Top3     float64            `json:"top3_accuracy"`
	ByMargin []EvalBucket       `json:"by_min_margin"`
	PerClass map[string]float64 `json:"top1_by_category"`
}

// EvalBucket is accuracy/coverage for predictions with margin >= MinMargin.
type EvalBucket struct {
	MinMargin float64 `json:"min_margin"`
	Coverage  float64 `json:"coverage"`
	Accuracy  float64 `json:"accuracy"`
}

// Eval measures image-only zero-shot accuracy on assets whose category the
// importer already inferred, to calibrate ConfidentMargin.
func (e *Engine) Eval(ctx context.Context) (*EvalResult, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("embedding server unavailable: %s", e.statusHint())
	}
	sc, err := e.newScorer(ctx, false)
	if err != nil {
		return nil, err
	}
	assets, err := e.st.All()
	if err != nil {
		return nil, err
	}
	type pred struct {
		ok     bool
		margin float64
	}
	var preds []pred
	var top1, top3 int
	perN, perOK := map[string]int{}, map[string]int{}
	for _, a := range assets {
		if a.Category == "" || sc.img.Row(a.RowID) == nil {
			continue
		}
		cats, _, _ := sc.suggest(a.RowID)
		ok := cats[0].Label == a.Category
		in3 := false
		for _, c := range cats[:3] {
			in3 = in3 || c.Label == a.Category
		}
		if ok {
			top1++
			perOK[a.Category]++
		}
		if in3 {
			top3++
		}
		perN[a.Category]++
		preds = append(preds, pred{ok, cats[0].Score - cats[1].Score})
	}
	n := len(preds)
	r := &EvalResult{Assets: n, PerClass: map[string]float64{}}
	if n == 0 {
		return r, nil
	}
	r.Top1 = round3(float64(top1) / float64(n))
	r.Top3 = round3(float64(top3) / float64(n))
	for _, m := range []float64{0, 0.01, 0.02, 0.03, 0.035, 0.04, 0.05} {
		var cov, ok int
		for _, p := range preds {
			if p.margin >= m {
				cov++
				if p.ok {
					ok++
				}
			}
		}
		b := EvalBucket{MinMargin: m, Coverage: round3(float64(cov) / float64(n))}
		if cov > 0 {
			b.Accuracy = round3(float64(ok) / float64(cov))
		}
		r.ByMargin = append(r.ByMargin, b)
	}
	for c, cnt := range perN {
		r.PerClass[c] = round3(float64(perOK[c]) / float64(cnt))
	}
	return r, nil
}
