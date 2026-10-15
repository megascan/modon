// Package classify infers categories, art styles and tags for imported
// assets from file paths, mesh/material names and geometry statistics.
// These heuristics seed the index; agents can refine entries afterwards via
// the update_asset_metadata tool.
package classify

import (
	"path/filepath"
	"regexp"
	"sort"
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
	// "vehicle" is a land vehicle; air, space and water craft have their own
	// categories (and get a "vehicle" tag). Words whose meaning depends on
	// context (ship, shuttle, rocket, station) are in contextual.
	"car": "vehicle", "truck": "vehicle", "van": "vehicle", "bus": "vehicle", "bike": "vehicle",
	"motorcycle": "vehicle", "vehicle": "vehicle", "wagon": "vehicle", "kart": "vehicle",
	"tractor": "vehicle", "train": "vehicle", "tram": "vehicle", "locomotive": "vehicle", "forklift": "vehicle",
	"bicycle": "vehicle", "scooter": "vehicle", "taxi": "vehicle", "ambulance": "vehicle", "jeep": "vehicle",
	"airplane": "aircraft", "aircraft": "aircraft", "helicopter": "aircraft",
	"jet": "aircraft", "biplane": "aircraft", "airliner": "aircraft", "glider": "aircraft",
	"blimp": "aircraft", "zeppelin": "aircraft", "airship": "aircraft",
	"spaceship": "spacecraft", "starship": "spacecraft", "spacecraft": "spacecraft", "ufo": "spacecraft",
	"satellite": "spacecraft", "starfighter": "spacecraft", "dropship": "spacecraft", "mothership": "spacecraft",
	"boat": "watercraft", "sailboat": "watercraft", "submarine": "watercraft", "canoe": "watercraft",
	"kayak": "watercraft", "raft": "watercraft", "yacht": "watercraft", "ferry": "watercraft",
	"rowboat": "watercraft", "gondola": "watercraft", "jetski": "watercraft", "watercraft": "watercraft",
	"sword": "weapon", "axe": "weapon", "hammer": "weapon", "gun": "weapon", "rifle": "weapon",
	"pistol": "weapon", "shotgun": "weapon", "bow": "weapon", "weapon": "weapon", "dagger": "weapon",
	"knife": "weapon", "staff": "weapon", "wand": "weapon", "spear": "weapon",
	"shield": "armor", "helmet": "armor", "armor": "armor", "armour": "armor",
	"character": "character", "human": "character", "man": "character", "woman": "character",
	"girl": "character", "boy": "character", "people": "character", "person": "character",
	"hero": "character", "npc": "character", "rigged": "character",
	// Pieces of a character rather than a whole one.
	"hair": "character-part", "hairstyle": "character-part", "beard": "character-part",
	"moustache": "character-part", "mustache": "character-part", "eyebrow": "character-part",
	"eyebrows": "character-part", "eyelashes": "character-part", "ponytail": "character-part",
	"decoration": "decoration", "ornament": "decoration", "garland": "decoration", "wreath": "decoration",
	"bauble": "decoration", "tinsel": "decoration",
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

	res.Category = inferCategory(path, dirTokens, append(append([]string{}, m.MeshNames...), m.MaterialNames...))

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
		if extra, ok := categoryTags[res.Category]; ok {
			add(extra)
		}
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

// tokenize splits a path/name into lowercase words; CamelCase parts become
// words of their own ("SpaceShip" -> space, ship).
func tokenize(s string) []string {
	var out []string
	for _, seg := range segments(s) {
		out = append(out, seg...)
	}
	return out
}

// segments splits a file or directory name at separators, then each
// separator-delimited segment at CamelCase and letter/digit boundaries:
// "SM_Prop_WallLantern_01" -> [sm] [prop] [wall lantern] [01].
func segments(s string) [][]string {
	base := filepath.Base(s)
	if ext := filepath.Ext(base); len(ext) > 1 && len(ext) <= 5 {
		base = strings.TrimSuffix(base, ext)
	}
	f := strings.FieldsFunc(base, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	out := make([][]string, 0, len(f))
	for _, w := range f {
		w = camelBoundary.ReplaceAllString(w, "$1 $2")
		w = acronymBoundary.ReplaceAllString(w, "$1 $2")
		w = digitBoundary.ReplaceAllString(w, "$1 $2")
		w = digitBoundary2.ReplaceAllString(w, "$1 $2")
		var parts []string
		for _, p := range strings.Fields(strings.ToLower(w)) {
			parts = append(parts, p)
		}
		if len(parts) > 0 {
			out = append(out, parts)
		}
	}
	return out
}

var (
	camelBoundary   = regexp.MustCompile(`([a-z])([A-Z])`)
	acronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	digitBoundary   = regexp.MustCompile(`([A-Za-z])([0-9])`)
	digitBoundary2  = regexp.MustCompile(`([0-9])([A-Za-z])`)
)

// ---- category inference ----

// categoryTags are extra tags for categories that refine a broader one, so
// filtering by the broad word still finds them.
var categoryTags = map[string]string{
	"aircraft": "vehicle", "spacecraft": "vehicle", "watercraft": "vehicle",
	"character-part": "character", "decoration": "prop",
}

// phrases are two-word names whose meaning differs from either word.
var phrases = map[[2]string]string{
	{"xmas", "tree"}: "decoration", {"christmas", "tree"}: "decoration",
	{"space", "ship"}: "spacecraft", {"space", "station"}: "spacecraft", {"space", "craft"}: "spacecraft",
	{"jet", "ski"}: "watercraft", {"tree", "house"}: "building", {"tree", "stump"}: "plant",
	{"street", "light"}: "lamp", {"street", "lamp"}: "lamp",
}

// Context word sets that disambiguate words like "ship" and "rocket". They
// are matched against every word of the file name and its folders.
var (
	spaceContext = set("space", "spaceship", "starship", "spacecraft", "galactic", "galaxy", "orbital",
		"orbit", "planet", "planetary", "asteroid", "satellite", "ufo", "starfighter", "interceptor",
		"dropship", "mothership")
	weaponContext    = set("wep", "weapon", "weapons", "gun", "guns", "rpg", "launcher", "ammo", "bazooka", "artillery")
	airContext       = set("veh", "vehicle", "vehicles", "air", "airport", "airfield", "aviation", "flight", "propeller", "aircraft", "airplane", "wing", "wings", "cockpit")
	characterContext = set("chr", "character", "characters", "human", "humans", "people", "npc", "avatar")
)

// contextual resolves words whose category depends on the surroundings.
var contextual = map[string]func(ctx map[string]bool) string{
	"ship": func(ctx map[string]bool) string {
		if spaceCtx(ctx) || scifiCtx(ctx) {
			return "spacecraft"
		}
		return "watercraft"
	},
	"shuttle": func(ctx map[string]bool) string { // a badminton shuttle is not a spacecraft
		if spaceCtx(ctx) || scifiCtx(ctx) {
			return "spacecraft"
		}
		return ""
	},
	"station": func(ctx map[string]bool) string { // gas and train stations are not
		if spaceCtx(ctx) {
			return "spacecraft"
		}
		return ""
	},
	"rocket": func(ctx map[string]bool) string {
		if anyIn(ctx, weaponContext) {
			return "weapon"
		}
		return "spacecraft"
	},
	"missile": func(map[string]bool) string { return "weapon" },
	"plane": func(ctx map[string]bool) string { // also a flat quad: "PuddlePlane", "Ground_Plane"
		if anyIn(ctx, airContext) {
			return "aircraft"
		}
		return ""
	},
}

// partMarkers name a piece of a character when the name or folder is about
// characters ("SM_Chr_Attach_..."); they lose to any concrete noun.
var partMarkers = set("attach", "attachment", "attachments", "accessory", "accessories", "head", "heads",
	"torso", "arm", "arms", "leg", "legs", "hand", "hands", "face", "eyes", "body", "bodypart", "parts")

// nameCodes are the asset-kind abbreviations of common library naming
// conventions ("SM_Veh_...", "SK_Chr_..."), used when no noun decides.
var nameCodes = map[string]string{"chr": "character", "veh": "vehicle", "wep": "weapon", "bld": "building"}

func set(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

func anyIn(ctx, words map[string]bool) bool {
	for w := range words {
		if ctx[w] {
			return true
		}
	}
	return false
}

// spaceCtx is explicit outer-space vocabulary; sci-fi alone is not enough
// (sci-fi kits are full of cars and tanks), except for ships.
func spaceCtx(ctx map[string]bool) bool { return anyIn(ctx, spaceContext) }

func scifiCtx(ctx map[string]bool) bool {
	return ctx["scifi"] || ctx["futuristic"] || ctx["sci"] && ctx["fi"]
}

// wordCategory maps one word to a category, consulting the context for
// ambiguous words.
func wordCategory(w string, ctx map[string]bool) string {
	w = lookup(w)
	if f, ok := contextual[w]; ok {
		return f(ctx)
	}
	return categoryKeywords[w]
}

// inferCategory picks a category from, in order of trust: two-word phrases
// in the file name, file-name nouns (separator segments left to right, as
// library names go general to specific, but within a CamelCase segment the
// last word first, as in "WallLantern"), character-part markers and
// naming-convention codes, folder names from the import root down, and
// finally mesh and material names.
func inferCategory(path string, dirs, inner []string) string {
	file := segments(path)
	ctx := map[string]bool{}
	var flat []string
	for _, seg := range file {
		for _, w := range seg {
			ctx[w] = true
			flat = append(flat, w)
		}
	}
	for _, d := range dirs {
		for _, w := range tokenize(d) {
			ctx[w] = true
		}
	}
	if c := vehicleRefine(phraseCategory(flat), ctx); c != "" {
		return c
	}
	for _, seg := range file {
		for i := len(seg) - 1; i >= 0; i-- {
			if c := wordCategory(seg[i], ctx); c != "" {
				return vehicleRefine(c, ctx)
			}
		}
	}
	charCtx := anyIn(ctx, characterContext)
	for _, w := range flat {
		if charCtx && partMarkers[w] {
			return "character-part"
		}
	}
	for _, w := range flat {
		if c, ok := nameCodes[w]; ok {
			return vehicleRefine(c, ctx)
		}
	}
	if charCtx {
		for _, d := range dirs {
			for _, w := range tokenize(d) {
				if partMarkers[w] {
					return "character-part"
				}
			}
		}
	}
	for _, d := range dirs {
		segs := segments(d)
		var words []string
		for _, seg := range segs {
			words = append(words, seg...)
		}
		if c := phraseCategory(words); c != "" {
			return c
		}
		for _, seg := range segs {
			for j := len(seg) - 1; j >= 0; j-- {
				if c := wordCategory(seg[j], ctx); c != "" {
					return vehicleRefine(c, ctx)
				}
			}
		}
	}
	for _, n := range inner {
		for _, w := range tokenize(n) {
			if c := wordCategory(w, ctx); c != "" {
				return c
			}
		}
	}
	return ""
}

func phraseCategory(words []string) string {
	for i := 0; i+1 < len(words); i++ {
		if c, ok := phrases[[2]string{lookup(words[i]), lookup(words[i+1])}]; ok {
			return c
		}
	}
	return ""
}

// vehicleRefine turns a generic "vehicle" into a space or water craft when
// the context says so ("SM_Veh_Fighter" in a space kit).
func vehicleRefine(c string, ctx map[string]bool) string {
	if c == "vehicle" && spaceCtx(ctx) && !anyIn(ctx, set("car", "truck", "van", "bus", "bike", "motorcycle", "train", "tram")) {
		return "spacecraft"
	}
	return c
}

// CategoryVocabulary returns each canonical category with the keywords that
// map to it (sorted), for zero-shot labelling.
func CategoryVocabulary() map[string][]string {
	out := map[string][]string{}
	for kw, c := range categoryKeywords {
		out[c] = append(out[c], kw)
	}
	for c := range out {
		sort.Strings(out[c])
	}
	return out
}
