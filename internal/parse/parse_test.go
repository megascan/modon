package parse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOBJCounts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chair.obj")
	content := "o Chair\nv 0 0 0\nv 1 0 0\nv 0 1 0\nusemtl Wood\nf 1 2 3\nf 1 2 3 4\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.VertCount != 3 {
		t.Fatalf("verts=%d", m.VertCount)
	}
	if m.TriCount != 3 { // 1 tri + 1 quad (2 tris)
		t.Fatalf("tris=%d", m.TriCount)
	}
	if len(m.MeshNames) == 0 || m.MeshNames[0] != "Chair" {
		t.Fatalf("meshnames=%v", m.MeshNames)
	}
}

func TestGLBJSONChunk(t *testing.T) {
	doc := `{"meshes":[{"name":"Cube","primitives":[{"attributes":{"POSITION":0},"indices":1}]}],"accessors":[{"count":8,"type":"VEC3"},{"count":36,"type":"SCALAR"}],"materials":[{"name":"Wood"}],"images":[{},{}],"animations":[{"name":"Idle","channels":[{}]}]}`
	m := Metadata{}
	parseGLTF("x.glb", strings.NewReader(wrapGLB([]byte(doc))), &m)
	if m.VertCount != 8 {
		t.Fatalf("verts=%d", m.VertCount)
	}
	if m.TriCount != 12 {
		t.Fatalf("tris=%d", m.TriCount)
	}
	if m.TextureCount != 2 {
		t.Fatalf("textures=%d", m.TextureCount)
	}
	if len(m.AnimationNames) == 0 {
		t.Fatal("expected animation")
	}
}

func wrapGLB(doc []byte) string {
	// Minimal GLB: magic + version + length + JSON chunk header + doc.
	header := make([]byte, 20)
	copy(header[0:4], "glTF")
	header[4] = 2 // version
	chunkLen := len(doc)
	header[12] = byte(chunkLen)
	header[13] = byte(chunkLen >> 8)
	header[14] = byte(chunkLen >> 16)
	header[15] = byte(chunkLen >> 24)
	copy(header[16:20], "JSON")
	return string(header) + string(doc)
}

func TestDisplayName(t *testing.T) {
	if got := DisplayName("/a/my-cool_chair.obj"); got != "my cool chair" {
		t.Fatalf("got %q", got)
	}
}
