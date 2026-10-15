package classify

import (
	"testing"

	"modon/internal/parse"
)

func TestInferChairLowpoly(t *testing.T) {
	m := parse.Metadata{
		MeshNames:     []string{"Chair"},
		MaterialNames: []string{"Wood"},
		TriCount:      800,
		Format:        "obj",
	}
	res := Infer("/assets/lowpoly_wooden_chair.obj", []string{"furniture"}, m)
	if res.Category != "chair" {
		t.Fatalf("category=%q", res.Category)
	}
	if res.Style == "" || !contains(res.Style, "lowpoly") {
		t.Fatalf("style=%q", res.Style)
	}
	if !hasTag(res.Tags, "chair") || !hasTag(res.Tags, "wood") {
		t.Fatalf("tags=%v", res.Tags)
	}
}

func TestInferHighPoly(t *testing.T) {
	m := parse.Metadata{TriCount: 500000, Format: "fbx"}
	res := Infer("/assets/dragon.fbx", nil, m)
	if res.Category != "creature" {
		t.Fatalf("category=%q", res.Category)
	}
	if !contains(res.Style, "high-poly") {
		t.Fatalf("style=%q", res.Style)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func TestInferCategoryCases(t *testing.T) {
	for _, c := range []struct {
		path string
		dirs []string
		want string
	}{
		// Parts of characters are not whole characters.
		{"/lib/SK_Chr_Hair_01.fbx", nil, "character-part"},
		{"/lib/SM_Chr_Attach_Hair_Long_01.fbx", []string{"Characters", "Attachments"}, "character-part"},
		{"/lib/hair_short_02.gltf", []string{"characters"}, "character-part"},
		{"/lib/SM_Chr_Attach_Visor_01.fbx", []string{"Characters"}, "character-part"},
		{"/lib/SM_Chr_Attach_Hat_01.fbx", []string{"Characters"}, "clothing"},
		{"/lib/SK_Chr_Soldier_01.fbx", nil, "character"},
		{"/lib/SK_Chr_Soldier_01.fbx", []string{"Characters"}, "character"},
		// Phrases beat their words.
		{"/lib/SM_Prop_Xmas_Tree_02.fbx", nil, "decoration"},
		{"/lib/SM_Prop_ChristmasTree_01.fbx", nil, "decoration"},
		{"/lib/SM_Env_Tree_Pine_01.fbx", nil, "tree"},
		// Vehicles split into land, air, space and water without false
		// watercraft hits.
		{"/lib/SM_Ship_Station_06.fbx", []string{"lowpoly-pack - Space Kit"}, "spacecraft"},
		{"/lib/SM_Ship_Fighter_01.fbx", []string{"Sci-Fi Kit"}, "spacecraft"},
		{"/lib/interceptor_ship.glb", nil, "spacecraft"},
		{"/lib/SM_Prop_SpaceShip_01.fbx", nil, "spacecraft"},
		{"/lib/SM_Veh_Boat_Large_01.fbx", nil, "watercraft"},
		{"/lib/SM_Prop_Pirate_Ship_01.fbx", nil, "watercraft"},
		{"/lib/SM_Veh_Car_01.fbx", []string{"Sci-Fi City Kit"}, "vehicle"},
		{"/lib/SM_Veh_Tank_01.fbx", []string{"Sci-Fi Kit"}, "vehicle"},
		{"/lib/SM_Veh_Fighter_01.fbx", []string{"Space Kit"}, "spacecraft"},
		{"/lib/SM_Veh_Helicopter_01.fbx", nil, "aircraft"},
		{"/lib/SM_Wep_RPG_Rocket_01.fbx", nil, "weapon"},
		{"/lib/SM_Prop_Rocket_01.fbx", nil, "spacecraft"},
		{"/lib/SM_Prop_Badminton_Shuttle_01.fbx", []string{"Sports"}, ""},
		{"/lib/SM_Bld_Station_Platform_01.fbx", []string{"Railway Kit"}, "building"}, // the naming code decides
		// CamelCase compounds: the last word is the head noun.
		{"/lib/SM_Prop_WallLantern_01.fbx", nil, "lamp"},
		{"/lib/SM_SignBorder_Ship_01.fbx", nil, "sign"},
		{"/lib/SM_Crate_01.fbx", []string{"Props", "Containers"}, "container"},
	} {
		if got := Infer(c.path, c.dirs, parse.Metadata{Format: "fbx"}).Category; got != c.want {
			t.Errorf("%s %v: category %q, want %q", c.path, c.dirs, got, c.want)
		}
	}
}

func TestRefinedCategoriesKeepTheBroadTag(t *testing.T) {
	res := Infer("/lib/SM_Veh_Helicopter_01.fbx", nil, parse.Metadata{Format: "fbx"})
	if !hasTag(res.Tags, "aircraft") || !hasTag(res.Tags, "vehicle") {
		t.Fatalf("tags = %v", res.Tags)
	}
	res = Infer("/lib/SK_Chr_Hair_01.fbx", nil, parse.Metadata{Format: "fbx"})
	if !hasTag(res.Tags, "character-part") || !hasTag(res.Tags, "character") {
		t.Fatalf("tags = %v", res.Tags)
	}
}

func TestPlaneNeedsAnAirContext(t *testing.T) {
	for path, want := range map[string]string{
		"/lib/SM_Env_PuddlePlane_01.fbx": "",
		"/lib/SM_Veh_Plane_01.fbx":       "aircraft",
		"/lib/SM_Prop_Airplane_01.fbx":   "aircraft",
	} {
		if got := Infer(path, nil, parse.Metadata{}).Category; got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	// Folders count from the import root; part folders still mark parts.
	if got := Infer("/lib/bartender.blend", []string{"characters", "wardrobe"}, parse.Metadata{}).Category; got != "character" {
		t.Errorf("root folder first: %q", got)
	}
}
