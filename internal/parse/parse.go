// Package parse extracts lightweight metadata from 3D asset files: names,
// triangle counts, materials, textures and animations. All parsers are
// best-effort; a file that fails to parse still yields filename-based
// metadata so it can be imported.
package parse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Metadata is the result of inspecting an asset file.
type Metadata struct {
	Name            string // preferred display name (filename minus extension)
	Format          string // extension without dot, e.g. "glb"
	FileSize        int64
	TriCount        int64
	VertCount       int64
	MeshNames       []string
	MaterialNames   []string
	TextureCount    int
	AnimationNames  []string
	EmbeddedPreview []byte // JPEG/PNG bytes found inside the file, if any
	RelatedFiles    []string
	Partial         bool // true if the file was only partially understood
	Notes           []string
}

// Extensions of 3D asset files modon understands.
var Extensions = map[string]bool{
	".gltf": true, ".glb": true, ".fbx": true, ".obj": true,
	".stl": true, ".ply": true, ".dae": true, ".blend": true,
	".usdz": true, ".usda": true, ".usdc": true, ".usd": true,
	".abc": true, ".3ds": true,
}

// DisplayName derives a human title from a path's basename.
func DisplayName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")
	// Collapse whitespace.
	fields := strings.Fields(name)
	return strings.Join(fields, " ")
}

// File parses path according to its extension. It never returns an error for
// malformed content unless the file cannot be read at all; parse problems are
// recorded in the returned Metadata.
func File(path string) (Metadata, error) {
	m := Metadata{Name: DisplayName(path)}
	fi, err := os.Stat(path)
	if err != nil {
		return m, err
	}
	m.FileSize = fi.Size()
	ext := strings.ToLower(filepath.Ext(path))
	m.Format = strings.TrimPrefix(ext, ".")

	data, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer data.Close()

	switch ext {
	case ".gltf", ".glb":
		parseGLTF(path, data, &m)
	case ".obj":
		parseOBJ(path, data, &m)
	case ".fbx":
		parseFBX(data, &m)
	case ".stl":
		parseSTL(data, &m)
	case ".ply":
		parsePLY(data, &m)
	case ".dae":
		parseDAE(data, &m)
	default:
		m.Partial = true
		m.Notes = append(m.Notes, fmt.Sprintf("no dedicated parser for %s files; metadata from filename", m.Format))
	}
	m.RelatedFiles = findRelated(path)
	return m, nil
}

func findRelated(path string) []string {
	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	lower := strings.ToLower(base)
	var rel []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		l := strings.ToLower(name)
		if l == strings.ToLower(filepath.Base(path)) {
			continue
		}
		// Same-basename files (.mtl, .bin) and sibling texture folders.
		if strings.HasPrefix(l, lower+".") || strings.HasPrefix(l, lower+"_") {
			rel = append(rel, filepath.Join(dir, name))
			continue
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".mtl", ".bin":
			rel = append(rel, filepath.Join(dir, name))
		}
	}
	return rel
}

func addUnique(list *[]string, vals ...string) {
	seen := map[string]bool{}
	for _, v := range *list {
		seen[strings.ToLower(v)] = true
	}
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" || seen[strings.ToLower(v)] {
			continue
		}
		seen[strings.ToLower(v)] = true
		*list = append(*list, v)
	}
}

// FBX "Namespace::Name\x00\x01Class" split helper.
func splitFBXName(s string) string {
	if i := strings.Index(s, "\x00\x01"); i > 0 {
		return s[:i]
	}
	return s
}
