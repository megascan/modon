// Package classify infers categories, art styles and tags for imported
// assets from file paths, mesh/material names and geometry statistics.
// These heuristics seed the index; agents can refine entries afterwards via
// the update_asset_metadata tool.
package classify

import (
	"path/filepath"
	"strings"

	"modon/internal/parse"
)

// categoryKeywords maps filename/path/mesh tokens to a canonical category.
var categoryKeywords = map[string]string{
	"chair": "chair", "stool": "chair", "armchair": "chair", "bench": "chair", "seat": "chair",
	"couch": "furniture", "sofa": "furniture", "table": "furniture", "desk": "furniture",
	"shelf": "furniture", "cabinet": "furniture", "wardrobe": "furniture", "bed": "furniture",
	"furniture": "furniture",
	"lamp":      "lamp", "lantern": "lamp", "streetlamp": "lamp",
	"car": "vehicle", "truck": "vehicle", "van": "vehicle", "bus": "vehicle", "bike": "vehicle",
	"motorcycle": "vehicle", "vehicle": "vehicle", "wagon": "vehicle", "kart": "vehicle",
	"plane": "aircraft", "airplane": "aircraft", "aircraft": "aircraft", "helicopter": "aircraft",
	"jet":    "aircraft",
	"rocket": "spacecraft", "spaceship": "spacecraft", "starship": "spacecraft", "shuttle": "spacecraft",
	"boat": "watercraft", "ship": "watercraft", "sailboat": "watercraft", "submarine": "watercraft",
	"sword": "weapon", "axe": "weapon", "hammer": "weapon", "gun": "weapon", "rifle": "weapon",
	"pistol": "weapon", "shotgun": "weapon", "bow": "weapon", "weapon": "weapon", "dagger": "weapon",
	"knife": "weapon", "staff": "weapon", "wand": "weapon", "spear": "weapon",
	"shield": "armor", "helmet": "armor", "armor": "armor", "armour": "armor",
	"character": "character", "human": "character", "man": "character", "woman": "character",
	"girl": "character", "boy": "character", "people": "character", "person": "character",
	"hero": "character", "npc": "character", "rigged": "character",
	"monster": "creature", "dragon": "creature", "creature": "creature", "alien": "creature",
	"zombie": "creature", "beast": "creature", "slime": "creature",
	"dog": "animal", "cat": "animal", "horse": "animal", "cow": "animal", "bird": "animal",
	"fish": "animal", "sheep": "animal", "pig": "animal", "chicken": "animal", "fox": "animal",
	"wolf": "animal", "bear": "animal", "deer": "animal", "frog": "animal",
	"tree": "tree", "palm": "tree", "pine": "tree",
	"bush": "plant", "plant": "plant", "flower": "plant", "grass": "plant", "cactus": "plant", "mushroom": "plant",
	"rock": "rock", "stone": "rock", "boulder": "rock", "cliff": "rock",
	"house": "building", "home": "building", "building": "building", "castle": "building",
	"tower": "building", "church": "building", "temple": "building", "cottage": "building",
	"skyscraper": "building", "barn": "building", "shop": "building", "ruin": "building",
	"hut": "building", "cabin": "building", "tent": "building",
	"door": "door", "window": "window", "gate": "door", "fence": "fence", "wall": "wall",
	"bridge": "environment", "stairs": "environment", "staircase": "environment",
	"road": "environment", "street": "environment", "traffic": "environment",
	"food": "food", "apple": "food", "banana": "food", "bread": "food", "cake": "food",
	"pizza": "food", "burger": "food", "cheese": "food", "fruit": "food", "vegetable": "food",
	"drink":  "food",
	"bottle": "container", "barrel": "container", "crate": "container", "box": "container",
	"chest": "container", "pot": "container", "vase": "container", "canister": "container", "bag": "container",
	"potion": "container",
	"tv":     "electronics", "computer": "electronics", "laptop": "electronics", "monitor": "electronics",
	"phone": "electronics", "radio": "electronics", "camera": "electronics",
	"robot": "robot", "mech": "robot", "android": "robot", "drone": "electronics",
	"sign": "sign", "billboard": "sign",
	"scroll": "prop", "book": "prop", "torch": "prop", "key": "prop", "coin": "prop", "gems": "prop", "money": "prop",
	"shirt": "clothing", "pants": "clothing", "hat": "clothing", "shoe": "clothing",
	"boot": "clothing", "dress": "clothing", "cape": "clothing", "glove": "clothing",
	"backpack": "clothing", "jacket": "clothing",
}

// styleKeywords maps tokens to art-style descriptors.
var styleKeywords = map[string]string{
	"lowpoly": "lowpoly", "low": "lowpoly", "poly": "lowpoly", "low-poly": "lowpoly",
	"stylized": "stylized", "stylise": "stylized", "stylize": "stylized",
	"realistic": "realistic", "photoreal": "realistic", "photorealistic": "realistic",
	"pbr": "pbr", "voxel": "voxel", "pixel": "pixel", "minecraft": "voxel",
	"cartoon": "cartoon", "toon": "cartoon", "cel": "cel-shaded", "anime": "anime",
	"scifi": "scifi", "sci": "scifi", "fi": "scifi", "cyber": "cyberpunk", "cyberpunk": "cyberpunk",
	"fantasy": "fantasy", "medieval": "medieval", "middle-ages": "medieval",
	"modern": "modern", "contemporary": "modern", "retro": "retro", "vintage": "retro",
	"future": "futuristic", "futuristic": "futuristic", "steampunk": "steampunk",
	"handpainted": "handpainted", "hand-painted": "handpainted", "painted": "handpainted",
	"minimal": "minimalist", "minimalist": "minimalist",
	"hard": "hard-surface", "hardsurface": "hard-surface", "organic": "organic",
	"gameready": "game-ready", "game-ready": "game-ready", "modular": "modular",
	"kitbash": "kitbash", "ancient": "ancient", "egypt": "ancient", "egyptian": "ancient",
	"greek": "ancient", "roman": "ancient", "viking": "medieval", "oriental": "asian",
	"asian": "asian", "japanese": "asian", "chinese": "asian",
}

// materialKeywords extracts surface/material hints from material or mesh names.
var materialKeywords = []string{
	"wood", "metal", "steel", "iron", "gold", "silver", "copper", "bronze",
	"stone", "concrete", "brick", "marble", "granite", "glass", "fabric",
	"leather", "cloth", "plastic", "rubber", "paper", "ceramic", "clay",
	"ice", "snow", "water", "lava", "sand", "dirt", "mud", "grass",
}

var singular = map[string]string{
	"chairs": "chair", "tables": "table", "trees": "tree", "rocks": "rock", "houses": "building",
	"buildings": "building", "weapons": "weapon", "swords": "sword", "cars": "vehicle",
	"vehicles": "vehicle", "animals": "animal", "characters": "character", "creatures": "creature",
	"planes": "aircraft", "boats": "watercraft", "ships": "watercraft", "plants": "plant",
	"flowers": "plant", "bushes": "plant", "boxes": "container", "crates": "container",
	"barrels": "container", "bottles": "container", "doors": "door", "windows": "window",
	"stones": "rock", "dragons": "creature", "monsters": "creature", "birds": "animal",
	"lampposts": "lamp", "lamps": "lamp", "meds": "medical", "beds": "furniture",
	"people": "people",
}

// Result is the classification outcome for one asset.
type Result struct {
	Category  string
	Style     string
	Tags      []string
	AssetType string
}

// Infer derives metadata for an imported file. dirTokens are the parent
// directory names relative to the import root.
func Infer(path string, dirTokens []string, m parse.Metadata) Result {
	tokens := tokenize(path)
	for _, d := range dirTokens {
		tokens = append(tokens, tokenize(d)...)
	}
	for _, n := range append(append([]string{}, m.MeshNames...), m.MaterialNames...) {
		tokens = append(tokens, tokenize(n)...)
	}

	res := Result{AssetType: "model"}
	if m.VertCount == 0 && len(m.AnimationNames) > 0 {
		res.AssetType = "animation"
	}

	// Category: first keyword hit wins, scanning most-specific sources.
	for _, t := range tokens {
		if c, ok := categoryKeywords[lookup(t)]; ok {
			res.Category = c
			break
		}
	}

	// Style: accumulate matching descriptors.
	styleSet := map[string]bool{}
	var styles []string
	for _, t := range tokens {
		if s, ok := styleKeywords[lookup(t)]; ok && !styleSet[s] {
			styleSet[s] = true
			styles = append(styles, s)
		}
	}
	// Geometry-based style hints.
	if m.TriCount > 0 && m.TriCount <= 1500 && !styleSet["lowpoly"] {
		styles = append(styles, "lowpoly")
		styleSet["lowpoly"] = true
	}
	if m.TriCount > 100000 {
		styles = append(styles, "high-poly")
	}
	res.Style = strings.Join(styles, ", ")

	// Tags: styles, category, materials, format and content hints.
	tagSet := map[string]bool{}
	var tags []string
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || tagSet[t] {
			return
		}
		tagSet[t] = true
		tags = append(tags, t)
	}
	for _, s := range styles {
		add(s)
	}
	if res.Category != "" {
		add(res.Category)
	}
	matSet := map[string]bool{}
	for _, n := range m.MaterialNames {
		for _, kw := range materialKeywords {
			if strings.Contains(strings.ToLower(n), kw) && !matSet[kw] {
				matSet[kw] = true
				add(kw)
			}
		}
	}
	if m.TextureCount > 0 {
		add("textured")
	}
	if len(m.AnimationNames) > 0 {
		add("animated")
	}
	if res.AssetType == "animation" {
		add("animation")
	}
	if m.Format != "" {
		add(m.Format)
	}
	// A couple of distinctive mesh names make search hits much better.
	for i, n := range m.MeshNames {
		if i >= 3 {
			break
		}
		if len(n) > 2 && len(n) < 40 {
			add(n)
		}
	}
	res.Tags = tags
	return res
}

// lookup normalizes a token for vocab matching: lowercase, strip whitespace,
// collapse plural forms and separators.
func lookup(tok string) string {
	t := strings.ToLower(strings.TrimSpace(tok))
	t = strings.ReplaceAll(t, "-", "")
	t = strings.ReplaceAll(t, "_", "")
	if s, ok := singular[t]; ok {
		return s
	}
	return t
}

// tokenize splits a path/name into lowercase words.
func tokenize(s string) []string {
	base := filepath.Base(s)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	f := strings.FieldsFunc(base, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	out := make([]string, 0, len(f))
	for _, w := range f {
		w = strings.ToLower(w)
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}
