package classify

// TagFacet is one axis of the recommended tag vocabulary. Prompt turns a
// term into the zero-shot text it is matched with ("%s" is the term).
type TagFacet struct {
	Name   string   `json:"facet"`
	About  string   `json:"about"`
	Prompt string   `json:"-"`
	Terms  []string `json:"terms"`
}

// TagVocabulary is the recommended vocabulary for deep tagging: tags that
// say what an asset is made of, which setting it belongs to, what it is
// used for in a scene and its kitbash role, on top of the object nouns in
// its name. Agents may use other tags; these are the ones suggestions
// score and the docs recommend, so tags stay consistent across a library.
var TagVocabulary = []TagFacet{
	{
		Name: "material", About: "dominant visible material",
		Prompt: "a 3D model made of %s",
		Terms: []string{"wood", "metal", "steel", "iron", "rusty-metal", "gold", "brass", "copper", "stone", "marble",
			"concrete", "brick", "glass", "fabric", "leather", "plastic", "rubber", "paper", "cardboard", "ceramic",
			"rope", "bone", "ice", "crystal", "foliage"},
	},
	{
		Name: "theme", About: "era, genre or setting",
		Prompt: "a %s themed 3D model",
		Terms: []string{"medieval", "fantasy", "sci-fi", "futuristic", "cyberpunk", "steampunk", "modern", "retro",
			"industrial", "military", "post-apocalyptic", "horror", "western", "pirate", "viking", "ancient",
			"asian", "nautical", "farm", "urban", "suburban", "office", "kitchen", "nightclub", "holiday", "nature"},
	},
	{
		Name: "condition", About: "wear and state",
		Prompt: "a %s 3D model",
		Terms:  []string{"new", "clean", "worn", "dirty", "damaged", "broken", "destroyed", "rusty", "burnt", "overgrown"},
	},
	{
		Name: "use", About: "what the piece is in a scene",
		Prompt: "a 3D %s for a game level",
		Terms: []string{"prop", "wall", "floor", "ceiling", "roof", "door", "window", "stairs", "pillar", "beam",
			"debris", "signage", "decal", "lighting", "furniture", "container", "vegetation", "terrain", "weapon-part",
			"vehicle-part", "character-part", "pickup", "icon"},
	},
	{
		Name: "kitbash", About: "role when assembling scenes",
		Prompt: "a %s 3D model",
		Terms: []string{"hero-prop", "set-dressing", "modular-piece", "trim", "detail", "filler", "background",
			"landmark", "clutter"},
	},
}
