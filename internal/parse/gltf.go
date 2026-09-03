package parse

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

// glTF container (JSON + optional binary chunk) and .glb.

type gltfJSON struct {
	Asset     interface{} `json:"asset"`
	Nodes     []gltfNode  `json:"nodes"`
	Meshes    []gltfMesh  `json:"meshes"`
	Materials []struct {
		Name string `json:"name"`
	} `json:"materials"`
	Images []json.RawMessage `json:"images"`
	Texts  []struct {
		Name string `json:"name"`
	} `json:"textures"`
	Animations []struct {
		Name     string `json:"name"`
		Channels []any  `json:"channels"`
	} `json:"animations"`
	Accessors []struct {
		Count int64  `json:"count"`
		Type  string `json:"type"`
	} `json:"accessors"`
	Skins []any `json:"skins"`
}

type gltfNode struct {
	Name string `json:"name"`
	Mesh *int   `json:"mesh"`
	Skin *int   `json:"skin"`
}

type gltfMesh struct {
	Name       string `json:"name"`
	Primitives []struct {
		Attributes map[string]int `json:"attributes"`
		Indices    *int           `json:"indices"`
		Mode       *int           `json:"mode"`
	} `json:"primitives"`
}

func parseGLTF(path string, r io.Reader, m *Metadata) {
	buf, err := io.ReadAll(r)
	if err != nil {
		m.Partial = true
		return
	}
	var doc []byte
	if hasGLBMagic(buf) {
		doc = glbJSONChunk(buf)
	} else {
		doc = buf
	}
	if doc == nil {
		m.Partial = true
		m.Notes = append(m.Notes, "could not locate glTF JSON chunk")
		return
	}
	var g gltfJSON
	if err := json.Unmarshal(doc, &g); err != nil {
		m.Partial = true
		m.Notes = append(m.Notes, "glTF JSON parse failed: "+err.Error())
		return
	}

	for _, mesh := range g.Meshes {
		addUnique(&m.MeshNames, mesh.Name)
		for _, prim := range mesh.Primitives {
			mode := 4
			if prim.Mode != nil {
				mode = *prim.Mode
			}
			if pos, ok := prim.Attributes["POSITION"]; ok && int(pos) < len(g.Accessors) {
				m.VertCount += g.Accessors[pos].Count
			}
			if mode == 4 {
				if prim.Indices != nil && int(*prim.Indices) < len(g.Accessors) {
					m.TriCount += g.Accessors[*prim.Indices].Count / 3
				} else if m.VertCount > 0 {
					m.TriCount += m.VertCount / 3
				}
			}
		}
	}
	for _, mat := range g.Materials {
		addUnique(&m.MaterialNames, mat.Name)
	}
	m.TextureCount = len(g.Images)
	for _, anim := range g.Animations {
		name := anim.Name
		if name == "" && len(anim.Channels) > 0 {
			name = "animation"
		}
		addUnique(&m.AnimationNames, name)
	}
	// Animation weighting: nodes with skins imply rigging.
	skinned := 0
	for _, n := range g.Nodes {
		if n.Skin != nil {
			skinned++
		}
	}
	_ = skinned
	for _, n := range g.Nodes {
		addUnique(&m.MeshNames, nodeDisplayName(n.Name))
	}
}

// nodeDisplayName strips common node-numbering suffixes ("Chair.001") to
// avoid flooding the name list with duplicates.
func nodeDisplayName(s string) string {
	if len(s) > 4 && s[len(s)-4] == '.' && isDigits(s[len(s)-3:]) {
		return s[:len(s)-4]
	}
	return s
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

func hasGLBMagic(b []byte) bool {
	return len(b) >= 12 && b[0] == 'g' && b[1] == 'l' && b[2] == 'T' && b[3] == 'F'
}

// glbJSONChunk extracts the JSON chunk of a GLB container.
func glbJSONChunk(b []byte) []byte {
	if len(b) < 20 {
		return nil
	}
	// header: magic(4) version(4) length(4); chunk: len(4) type(4)
	chunkLen := binary.LittleEndian.Uint32(b[12:16])
	chunkType := string(b[16:20])
	if chunkType != "JSON" {
		return nil
	}
	if int(chunkLen) > len(b)-20 {
		chunkLen = uint32(len(b) - 20)
	}
	doc := b[20 : 20+chunkLen]
	// Trim trailing NUL padding that breaks strict JSON decoding.
	return bytes.TrimRight(doc, "\x00 \t\r\n")
}
