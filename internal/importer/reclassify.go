package importdir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"modon/internal/classify"
	"modon/internal/model"
	"modon/internal/parse"
	"modon/internal/store"
)

// ReclassifyOptions controls Reclassify.
type ReclassifyOptions struct {
	Apply    bool // write the changes (default: dry run)
	Limit    int  // cap on assets considered, 0 = all
	Progress func(done, total int)
}

// ReclassifyStats summarizes a reclassification run. Changes counts
// category moves ("character -> character-part"); Examples keeps a few
// titles for each.
type ReclassifyStats struct {
	Considered     int                 `json:"considered"`
	Changed        int                 `json:"changed"`
	CategoryMoves  int                 `json:"category_changes"`
	AgentProtected int                 `json:"agent_category_kept"`
	Unreadable     int                 `json:"unreadable_files"`
	Changes        map[string]int      `json:"changes_by_category"`
	Examples       map[string][]string `json:"examples,omitempty"`
	Applied        bool                `json:"applied"`
}

// Reclassify re-runs the import heuristics over the indexed assets, from
// their files (re-parsed for mesh names; the stored metadata is used when a
// file is unreadable), and refreshes heuristic metadata with the same rules
// as a re-import: agent-written fields are never touched, zero-shot values
// are replaced only by a real heuristic value, descriptions and the review
// flag stay. Folder words come from the path below the library pack folder,
// or the last three folders (never the home directory).
func Reclassify(ctx context.Context, st *store.Store, o ReclassifyOptions) (*ReclassifyStats, error) {
	assets, err := st.All()
	if err != nil {
		return nil, err
	}
	if o.Limit > 0 && len(assets) > o.Limit {
		assets = assets[:o.Limit]
	}
	results := make([]classify.Result, len(assets))
	readable := make([]bool, len(assets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i], readable[i] = inferStored(assets[i])
				if o.Progress != nil {
					mu.Lock()
					done++
					o.Progress(done, len(assets))
					mu.Unlock()
				}
			}
		}()
	}
	for i := range assets {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s := &ReclassifyStats{Changes: map[string]int{}, Examples: map[string][]string{}, Applied: o.Apply}
	for i, a := range assets {
		r := results[i]
		s.Considered++
		if !readable[i] {
			s.Unreadable++
		}
		agentCat := a.Provenance["category"] == model.ProvenanceAgent
		if agentCat && r.Category != "" && r.Category != a.Category {
			s.AgentProtected++
		}
		if !heuristicsDiffer(a, r) {
			continue
		}
		s.Changed++
		if !agentCat && r.Category != "" && r.Category != a.Category {
			s.CategoryMoves++
			key := orDash(a.Category) + " -> " + r.Category
			s.Changes[key]++
			if len(s.Examples[key]) < 3 {
				s.Examples[key] = append(s.Examples[key], fmt.Sprintf("%s %s", a.ID, a.Title))
			}
		}
		if o.Apply {
			if _, err := st.SetHeuristics(a.RowID, store.Heuristics{
				Category: r.Category, Style: r.Style, AssetType: r.AssetType, Tags: r.Tags,
			}); err != nil {
				return s, fmt.Errorf("%s: %w", a.ID, err)
			}
		}
	}
	return s, nil
}

// heuristicsDiffer reports whether SetHeuristics would change a field the
// importer owns.
func heuristicsDiffer(a model.Asset, r classify.Result) bool {
	owned := func(field string) bool { return a.Provenance[field] != model.ProvenanceAgent }
	if owned("category") && r.Category != "" && (r.Category != a.Category || a.Provenance["category"] != "") {
		return true
	}
	if owned("style") && r.Style != "" && (r.Style != a.Style || a.Provenance["style"] != "") {
		return true
	}
	if owned("asset_type") && r.AssetType != "" && (r.AssetType != a.AssetType || a.Provenance["asset_type"] != "") {
		return true
	}
	if owned("tags") {
		norm := func(ts []string) string {
			out := make([]string, 0, len(ts))
			for _, t := range ts {
				if t = strings.Join(strings.Fields(t), "-"); t != "" {
					out = append(out, t)
				}
			}
			return strings.Join(out, " ")
		}
		return norm(r.Tags) != norm(a.Tags)
	}
	return false
}

// inferStored classifies an indexed asset from its file, falling back to
// the stored parse results when the file cannot be read.
func inferStored(a model.Asset) (classify.Result, bool) {
	m, err := parse.File(a.PrimaryFile)
	ok := err == nil
	if !ok {
		m = parse.Metadata{
			MaterialNames: a.MaterialNames, TriCount: a.TriCount, VertCount: a.VertCount,
			TextureCount: a.TextureCount, AnimationNames: a.AnimationNames,
		}
		if len(a.Formats) > 0 {
			m.Format = a.Formats[0]
		}
	}
	return classify.Infer(a.PrimaryFile, ClassifyDirs(a.PrimaryFile), m), ok
}

var packFolder = regexp.MustCompile(`(?i) - |(^|[ _-])(pack|kit)([ _-]|$)`)

// ClassifyDirs are the folder names used as classification hints for an
// indexed file whose import root is no longer known: from the library pack
// folder down when the path has one, else the last three folders, never
// including the home directory or anything above it.
func ClassifyDirs(path string) []string {
	dir := filepath.ToSlash(filepath.Dir(path))
	if home, err := os.UserHomeDir(); err == nil {
		if h := filepath.ToSlash(home); dir == h || strings.HasPrefix(dir, h+"/") {
			dir = strings.TrimPrefix(strings.TrimPrefix(dir, h), "/")
		}
	}
	var segs []string
	for _, s := range strings.Split(dir, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	for i, s := range segs {
		if packFolder.MatchString(s) {
			return segs[i:]
		}
	}
	if len(segs) > 3 {
		segs = segs[len(segs)-3:]
	}
	return segs
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// SortedChanges lists category changes, most frequent first.
func (s *ReclassifyStats) SortedChanges() []string {
	keys := make([]string, 0, len(s.Changes))
	for k := range s.Changes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if s.Changes[keys[i]] != s.Changes[keys[j]] {
			return s.Changes[keys[i]] > s.Changes[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
