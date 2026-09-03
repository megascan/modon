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
