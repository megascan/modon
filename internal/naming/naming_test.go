package naming

import "testing"

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"SF Veh BatteringRam 01": "vehicle Battering Ram",
		"SM Prop WallLantern 06": "Prop Wall Lantern",
		"SM Wep HMGTripod 01":    "weapon HMG Tripod",
		"TV Stand":               "TV Stand",
		"01":                     "01",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPackKeyAndName(t *testing.T) {
	for _, c := range []struct{ path, key, name string }{
		{"/lib/Vendor/POLYGON - Harbor Pack/Harbor_SourceFiles_v2/FBX/SM_Crate_01.fbx", "/lib/Vendor/POLYGON - Harbor Pack", "Harbor"},
		{"/lib/Vendor/POLYGON - Harbor Pack/Harbor_SourceFiles_v2/OBJ/SM_Crate_01.obj", "/lib/Vendor/POLYGON - Harbor Pack", "Harbor"},
		{"/lib/Vendor/SIMPLE Orchard - Cartoon Assets/Obj/Barn.obj", "/lib/Vendor/SIMPLE Orchard - Cartoon Assets", "Orchard"},
		{"/game/assets/props/source/crate.fbx", "/game/assets/props", ""},
		{"/game/assets/props/crate.glb", "/game/assets/props", ""},
		{"/game/assets/props/variants/crate.glb", "/game/assets/props/variants", ""},
	} {
		if got := PackKey(c.path); got != c.key {
			t.Errorf("PackKey(%q) = %q, want %q", c.path, got, c.key)
		}
		if got := PackName(c.path); got != c.name {
			t.Errorf("PackName(%q) = %q, want %q", c.path, got, c.name)
		}
	}
}

func TestDupName(t *testing.T) {
	if DupName("SM_Prop_Chair_01") != DupName("SM Prop Chair 01") {
		t.Fatal("separators must not matter")
	}
	if DupName("Chair 01") == DupName("Chair 02") {
		t.Fatal("variant numbers must matter")
	}
}
