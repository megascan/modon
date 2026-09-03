package parse

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"math"
	"strings"
)

// Minimal FBX parser (binary 7.x + ASCII fallback) that recovers object
// names, geometry statistics, materials, textures, animation stacks and any
// embedded thumbnail. Malformed data degrades to filename heuristics.

type fbxProp struct {
	kind byte
	data any
}

type fbxNode struct {
	Name     string
	Props    []fbxProp
	Children []fbxNode
}

func (n *fbxNode) child(name string) *fbxNode {
	for i := range n.Children {
		if n.Children[i].Name == name {
			return &n.Children[i]
		}
	}
	return nil
}

func (n *fbxNode) walk(name string, fn func(*fbxNode)) {
	for i := range n.Children {
		if n.Children[i].Name == name {
			fn(&n.Children[i])
		}
		n.Children[i].walk(name, fn)
	}
}

func parseFBX(r io.Reader, m *Metadata) {
	buf, err := io.ReadAll(io.LimitReader(r, 2<<30))
	if err != nil {
		m.Partial = true
		return
	}
	if len(buf) > 23 && string(buf[:20]) == "Kaydara FBX Binary  " {
		parseFBXBinary(buf, m)
		return
	}
	if bytes.Contains(buf[:min(len(buf), 4096)], []byte("FBXHeaderExtension")) {
		parseFBXASCII(buf, m)
		return
	}
	m.Partial = true
	m.Notes = append(m.Notes, "unrecognized FBX container")
}

func parseFBXBinary(buf []byte, m *Metadata) {
	version := binary.LittleEndian.Uint32(buf[23:27])
	root := fbxNode{Name: "root"}
	pos := 27
	long := version >= 7500
	for pos < len(buf) {
		node, next, ok := readFBXNode(buf, pos, long)
		if !ok || next <= pos {
			break
		}
		pos = next
		root.Children = append(root.Children, *node)
	}
	collectFBX(&root, m)
}

func readFBXNode(buf []byte, pos int, long bool) (*fbxNode, int, bool) {
	start := pos
	width := 4
	if long {
		width = 8
	}
	need := 3*width + 1
	if pos+need > len(buf) {
		return nil, pos, false
	}
	if allZero(buf[pos : pos+need]) { // null record terminator (13 or 25 zero bytes)
		return nil, pos + need, false
	}
	readU := func() uint64 {
		if long {
			v := binary.LittleEndian.Uint64(buf[pos:])
			pos += 8
			return v
		}
		v := uint64(binary.LittleEndian.Uint32(buf[pos:]))
		pos += 4
		return v
	}
	endOffset := readU()
	numProps := readU()
	readU() // propertyListLen (not needed for decoding)
	nameLen := int(buf[pos])
	pos++
	if nameLen == 0 || pos+nameLen > int(endOffset) || int(endOffset) > len(buf) {
		return nil, start + 1, false
	}
	node := &fbxNode{Name: string(buf[pos : pos+nameLen])}
	pos += nameLen

	for i := uint64(0); i < numProps; i++ {
		prop, ok := readFBXProp(buf, &pos)
		if !ok {
			return node, int(endOffset), true
		}
		node.Props = append(node.Props, prop)
	}

	for pos < int(endOffset) {
		if allZero(buf[pos:min(pos+need, len(buf))]) {
			break
		}
		child, next, ok := readFBXNode(buf, pos, long)
		if !ok || next <= pos {
			break
		}
		pos = next
		node.Children = append(node.Children, *child)
	}
	return node, int(endOffset), true
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func readFBXProp(buf []byte, pos *int) (fbxProp, bool) {
	if *pos >= len(buf) {
		return fbxProp{}, false
	}
	kind := buf[*pos]
	*pos++
	switch kind {
	case 'Y': // int16
		if *pos+2 > len(buf) {
			return fbxProp{}, false
		}
		v := int16(binary.LittleEndian.Uint16(buf[*pos:]))
		*pos += 2
		return fbxProp{kind, v}, true
	case 'C': // bool
		if *pos+1 > len(buf) {
			return fbxProp{}, false
		}
		v := buf[*pos] != 0
		*pos++
		return fbxProp{kind, v}, true
	case 'I': // int32
		if *pos+4 > len(buf) {
			return fbxProp{}, false
		}
		v := int32(binary.LittleEndian.Uint32(buf[*pos:]))
		*pos += 4
		return fbxProp{kind, v}, true
	case 'F': // float32
		if *pos+4 > len(buf) {
			return fbxProp{}, false
		}
		v := math.Float32frombits(binary.LittleEndian.Uint32(buf[*pos:]))
		*pos += 4
		return fbxProp{kind, v}, true
	case 'D': // float64
		if *pos+8 > len(buf) {
			return fbxProp{}, false
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(buf[*pos:]))
		*pos += 8
		return fbxProp{kind, v}, true
	case 'L': // int64
		if *pos+8 > len(buf) {
			return fbxProp{}, false
		}
		v := int64(binary.LittleEndian.Uint64(buf[*pos:]))
		*pos += 8
		return fbxProp{kind, v}, true
	case 'f', 'd', 'l', 'i', 'b': // arrays
		if *pos+12 > len(buf) {
			return fbxProp{}, false
		}
		arrLen := int(binary.LittleEndian.Uint32(buf[*pos:]))
		encoding := binary.LittleEndian.Uint32(buf[*pos+4:])
		compLen := int(binary.LittleEndian.Uint32(buf[*pos+8:]))
		*pos += 12
		if *pos+compLen > len(buf) {
			return fbxProp{}, false
		}
		raw := buf[*pos : *pos+compLen]
		*pos += compLen
		if encoding == 1 {
			zr, err := zlib.NewReader(bytes.NewReader(raw))
			if err != nil {
				return fbxProp{kind, nil}, true
			}
			dec, err := io.ReadAll(zr)
			if err != nil {
				return fbxProp{kind, nil}, true
			}
			raw = dec
		}
		return fbxProp{kind, &fbxArray{len: arrLen, raw: raw}}, true
	case 'S', 'R': // string / raw
		if *pos+4 > len(buf) {
			return fbxProp{}, false
		}
		l := int(binary.LittleEndian.Uint32(buf[*pos:]))
		*pos += 4
		if *pos+l > len(buf) {
			return fbxProp{}, false
		}
		v := buf[*pos : *pos+l]
		*pos += l
		if kind == 'S' {
			return fbxProp{kind, string(v)}, true
		}
		return fbxProp{kind, v}, true
	default:
		return fbxProp{}, false
	}
}

type fbxArray struct {
	len int
	raw []byte
}

func collectFBX(root *fbxNode, m *Metadata) {
	objects := root.child("Objects")
	if objects != nil {
		for i := range objects.Children {
			n := &objects.Children[i]
			display := ""
			if len(n.Props) > 0 {
				if s, ok := n.Props[0].data.(string); ok {
					display = splitFBXName(s)
				}
			}
			switch n.Name {
			case "Model":
				addUnique(&m.MeshNames, display)
			case "Geometry":
				addUnique(&m.MeshNames, display)
				if v := n.propArrayLen("Vertices"); v > 0 {
					m.VertCount += int64(v / 3)
				}
				if idx := n.propArrayLen("PolygonVertexIndex"); idx > 0 {
					// Triangulated FBX files dominate; this is an estimate.
					m.TriCount += int64(idx) / 3
				}
			case "Material":
				addUnique(&m.MaterialNames, display)
			case "Video":
				// Embedded/linked textures are Video nodes with a Filename.
				if fn, ok := n.propString("Filename"); ok && fn != "" {
					m.TextureCount++
				} else if len(n.Props) > 0 {
					m.TextureCount++
				}
			case "AnimationStack":
				addUnique(&m.AnimationNames, display)
			case "AnimationLayer":
				// count only; names less useful
			}
		}
	}
	// Embedded thumbnail (some exporters include a preview image).
	takes := root.child("FileThumbnail")
	if takes != nil {
		t := takes.child("Thumbnail")
		if t != nil {
			for _, p := range t.Props {
				if b, ok := p.data.([]byte); ok {
					if len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 {
						m.EmbeddedPreview = b
					} else if len(b) > 4 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' {
						m.EmbeddedPreview = b
					}
				}
			}
		}
	}
	if m.VertCount == 0 && m.TriCount == 0 {
		m.Partial = true
	}
}

func (n *fbxNode) propString(name string) (string, bool) {
	c := n.child(name)
	if c == nil || len(c.Props) == 0 {
		return "", false
	}
	s, ok := c.Props[0].data.(string)
	return s, ok
}

func (n *fbxNode) propArrayLen(name string) int {
	c := n.child(name)
	if c == nil || len(c.Props) == 0 {
		return 0
	}
	if a, ok := c.Props[0].data.(*fbxArray); ok && a != nil {
		return a.len
	}
	return 0
}

func parseFBXASCII(buf []byte, m *Metadata) {
	s := string(buf)
	// Object declarations: e.g. `Model: 1234567, "Model::Cube", "Mesh" {`
	for _, kind := range []string{"Model", "Geometry", "Material"} {
		marker := kind + ": "
		rest := s
		for len(rest) > 0 {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			j := strings.Index(rest, `"`)
			if j < 0 {
				break
			}
			rest = rest[j+1:]
			k := strings.Index(rest, `"`)
			if k < 0 {
				break
			}
			name := splitFBXName(rest[:k])
			rest = rest[k+1:]
			switch kind {
			case "Model", "Geometry":
				addUnique(&m.MeshNames, name)
			case "Material":
				addUnique(&m.MaterialNames, name)
			}
		}
	}
	if strings.Contains(s, "AnimationStack") || strings.Contains(s, "Take: ") {
		addUnique(&m.AnimationNames, "fbx-take")
	}
	m.Partial = true
	m.Notes = append(m.Notes, "ASCII FBX: statistics not extracted")
}
