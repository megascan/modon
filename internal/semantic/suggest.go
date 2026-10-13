package semantic

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"modon/internal/classify"
	"modon/internal/embed"
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
	vecs, err := e.cl.Embed(ctx, ins)
	if err != nil {
		return nil, err
	}
	ls := &labelSet{}
	for i, c := range cats {
		ls.categories = append(ls.categories, label{c, vecs[i]})
	}
	for i, s := range styles {
		ls.styles = append(ls.styles, label{s, vecs[len(cats)+i]})
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

// Suggestion is one zero-shot label with its centered similarity.
type Suggestion struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// Suggestions are the zero-shot guesses for one asset. Margin is the gap
// between the first and second category; Confident marks margins above the
// threshold auto-classification uses.
type Suggestions struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Current    [2]string    `json:"current"` // category, style
	Categories []Suggestion `json:"categories"`
	Styles     []Suggestion `json:"styles"`
	Basis      []string     `json:"basis"` // image and/or text
	Margin     float64      `json:"margin"`
	Confident  bool         `json:"confident"`
}

// ConfidentMargin is the category score gap above which auto-classification
// fills an empty category (calibrated with `modon embed --eval`).
const ConfidentMargin = 0.05

type scorer struct {
	ls         *labelSet
	img, txt   *store.Matrix
	cImg, cTxt []float32
	useText    bool
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
		out[i] = Suggestion{l.name, float64(s) / float64(len(sources))}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func (sc *scorer) suggest(id int64) (cats, styles []Suggestion, basis []string) {
	var sources [][2][]float32
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
	if len(sources) == 0 {
		return nil, nil, nil
	}
	return scoreLabels(sc.ls.categories, sources), scoreLabels(sc.ls.styles, sources), basis
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func top(s []Suggestion, n int) []Suggestion {
	if len(s) > n {
		s = s[:n]
	}
	out := make([]Suggestion, len(s))
	for i, x := range s {
		out[i] = Suggestion{x.Label, round3(x.Score)}
	}
	return out
}

// Suggest returns zero-shot category/style guesses for a local asset from
// its preview image (and text document).
func (e *Engine) Suggest(ctx context.Context, id int64) (*Suggestions, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("suggestions need the embedding server (%s)", e.statusHint())
	}
	a, err := e.st.GetByID(id)
	if err != nil {
		return nil, err
	}
	e.Refresh(ctx, id) // incremental: embeds only if text/preview changed
	sc, err := e.newScorer(ctx, true)
	if err != nil {
		return nil, err
	}
	cats, styles, basis := sc.suggest(id)
	if cats == nil {
		return nil, fmt.Errorf("local:%d has no vectors yet; run `modon embed` (add --render for previews)", id)
	}
	margin := cats[0].Score - cats[1].Score
	return &Suggestions{
		ID: a.ID, Title: a.Title, Current: [2]string{a.Category, a.Style},
		Categories: top(cats, 3), Styles: top(styles, 3), Basis: basis,
		Margin: round3(margin), Confident: margin >= ConfidentMargin,
	}, nil
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
