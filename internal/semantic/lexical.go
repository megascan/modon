package semantic

import (
	"math"
	"sort"
	"strings"

	"modon/internal/model"
	"modon/internal/naming"
	"modon/internal/store"
)

// stopwords carry no meaning in asset queries ("gun on a tripod").
var stopwords = map[string]bool{"a": true, "an": true, "the": true, "of": true, "on": true, "in": true,
	"with": true, "for": true, "and": true, "or": true, "to": true, "at": true, "by": true, "from": true,
	"some": true, "that": true, "this": true, "is": true, "into": true, "like": true}

// queryTokens lowercases and splits a query, dropping stopwords and
// one-letter tokens.
func queryTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 128)
	}) {
		if len(t) > 1 && !stopwords[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// lexHit is one keyword candidate with its IDF-weighted coverage.
type lexHit struct {
	id    int64
	score float64 // 0..1+: name coverage, any-field coverage, phrase bonus
}

// Lexical ranking weights.
const (
	lexCandidates = 400  // FTS OR candidates scored per query
	nameShare     = 0.5  // share of coverage counted on the name (title) alone
	phraseBonus   = 0.25 // the whole query appears as a phrase in the name
)

// matchLevel credits a whole-word match fully and a prefix-only match
// ("work" in "workbench") partly.
var matchLevel = [3]float64{0, 0.4, 1}

// lexical ranks keyword candidates by how much of the query's information
// they cover: each query token weighs its inverse document frequency, so
// a document matching the rare word ("radioactive") outranks thousands
// that match only the common one ("barrel"). Matches in the name count more
// than matches in tags or descriptions, and the whole query as a phrase in
// the name earns a bonus. Results are best first.
func (e *Engine) lexical(q model.Query) ([]lexHit, error) {
	tokens := queryTokens(q.Text)
	if len(tokens) == 0 {
		return nil, nil
	}
	df, total, err := e.st.TokenDF(tokens)
	if err != nil {
		return nil, err
	}
	idf := map[string]float64{}
	var sum float64
	for _, t := range tokens {
		if df[t] == 0 {
			continue // matches nothing: cannot tell candidates apart
		}
		w := math.Log(float64(total+1) / float64(df[t]+1))
		if w < 0.1 {
			w = 0.1 // present everywhere: still counts a little
		}
		idf[t] = w
		sum += w
	}
	if sum == 0 {
		return nil, nil
	}
	cands, err := e.st.LexicalCandidates(tokens, filtersOnly(q), lexCandidates)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	ids := make([]int64, len(cands))
	for i, a := range cands {
		ids[i] = a.RowID
	}
	matches, err := e.st.TokenMatches(tokens, ids)
	if err != nil {
		return nil, err
	}
	phrase := strings.Join(tokens, " ")
	hits := make([]lexHit, len(cands))
	for i, a := range cands {
		var cov float64
		for _, t := range tokens {
			m := matches[t][a.RowID]
			cov += idf[t] * (nameShare*matchLevel[m.Name] + (1-nameShare)*matchLevel[m.Any])
		}
		s := cov / sum
		if len(tokens) > 1 && strings.Contains(" "+normPhrase(naming.CleanName(a.Title))+" ", " "+phrase+" ") {
			s += phraseBonus
		}
		hits[i] = lexHit{a.RowID, s}
	}
	// Stable: equal coverage keeps the BM25 order of the candidates.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	return hits, nil
}

func normPhrase(s string) string { return strings.Join(queryTokensKeepAll(s), " ") }

func queryTokensKeepAll(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 128)
	})
}

// filtersOnly keeps a query's facet filters, not its text.
func filtersOnly(q model.Query) model.Query {
	return model.Query{Category: q.Category, Style: q.Style, AssetType: q.AssetType, Animated: q.Animated, Tags: q.Tags}
}

// Fusion weights for the keyword side of hybrid search, calibrated on a
// 24.5k-asset library with descriptive and synonym queries (results were
// stable for lexBoost 0.06-0.15 and keywordWeight 1-1.5). One RRF signal
// at rank 1 is worth 1/61 ~ 0.016.
const (
	// keywordWeight is the RRF weight of the coverage-ranked keyword list.
	keywordWeight = 1.5
	// lexBoost is added in proportion to squared query coverage, so a
	// document naming the rare query words competes with documents only
	// the two vector signals like (a "radioactive" prop for "radioactive barrel").
	lexBoost = 0.1
	// lexAlone scales the boost for documents no vector signal found that
	// miss part of the query (a lone rare word, a stemming accident).
	lexAlone     = 0.25
	fullCoverage = 0.95
)

// keywordSignal ranks keyword matches by IDF coverage (falling back to the
// plain keyword search without FTS) and returns each candidate's coverage
// for lexicalBonus.
func (e *Engine) keywordSignal(q model.Query) (signal, map[int64]float64, error) {
	lex, err := e.lexical(q)
	if err != nil {
		return signal{}, nil, err
	}
	if lex == nil {
		kq := q
		kq.Limit = poolSize
		kw, mode, err := e.st.SearchMode(kq)
		if err != nil {
			return signal{}, nil, err
		}
		ids := make([]int64, len(kw))
		for i, a := range kw {
			ids[i] = a.RowID
		}
		// Loose keyword fallbacks (any token, substring) are mostly noise
		// for multi-word queries; the vectors carry those.
		w := keywordWeight
		if mode != store.MatchAll {
			w *= 0.35
		}
		return signal{"keyword", w, ids}, nil, nil
	}
	if len(lex) > poolSize {
		lex = lex[:poolSize]
	}
	ids := make([]int64, len(lex))
	cov := make(map[int64]float64, len(lex))
	for i, h := range lex {
		ids[i] = h.id
		cov[h.id] = h.score
	}
	return signal{"keyword", keywordWeight, ids}, cov, nil
}

// lexicalBonus turns coverage into an additive fusion score: lexBoost
// times the squared coverage. A document that only keyword search found
// (no text or image vector agrees) and that misses part of the query gets
// a fraction of it: one rare query word on an unrelated asset, or a
// stemming accident ("train" matching "training"), must not lead.
func lexicalBonus(cov map[int64]float64, signals []signal) map[int64]float64 {
	if len(cov) == 0 {
		return nil
	}
	vec := map[int64]bool{}
	for _, s := range signals {
		if s.name == "text" || s.name == "image" {
			for _, id := range s.ids {
				vec[id] = true
			}
		}
	}
	out := make(map[int64]float64, len(cov))
	for id, c := range cov {
		c = math.Min(c, 1)
		b := lexBoost * c * c
		if !vec[id] && c < fullCoverage {
			b *= lexAlone
		}
		out[id] = b
	}
	return out
}
