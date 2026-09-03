package parse

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Wavefront OBJ (+ sibling .mtl files).

func parseOBJ(path string, r io.Reader, m *Metadata) {
	sc := bufio.NewScanner(io.LimitReader(r, 64<<20))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var verts int64
	var faces int64
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "v "):
			verts++
		case strings.HasPrefix(line, "f "):
			n := strings.Fields(strings.TrimSpace(line[2:]))
			if len(n) >= 3 {
				faces++
				m.TriCount += int64(len(n) - 2)
			}
		case strings.HasPrefix(line, "o "), strings.HasPrefix(line, "g "):
			addUnique(&m.MeshNames, strings.TrimSpace(line[2:]))
		case strings.HasPrefix(line, "usemtl "):
			addUnique(&m.MaterialNames, strings.TrimSpace(line[7:]))
		}
	}
	m.VertCount = verts
	if err := sc.Err(); err != nil {
		m.Partial = true
		m.Notes = append(m.Notes, "obj scan aborted: "+err.Error())
	}

	// Count texture maps referenced by sibling .mtl files.
	for _, rel := range m.RelatedFiles {
		if strings.ToLower(filepath.Ext(rel)) != ".mtl" {
			continue
		}
		mtl, err := os.Open(rel)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(io.LimitReader(mtl, 8<<20))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "map_") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && !strings.HasPrefix(fields[len(fields)-1], "-") {
					m.TextureCount++
				}
			}
		}
		mtl.Close()
	}
	if len(m.MaterialNames) > 0 && m.TextureCount == 0 {
		m.Notes = append(m.Notes, "materials present but no texture maps referenced")
	}
}

// Stanford PLY header.
func parsePLY(r io.Reader, m *Metadata) {
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	if !sc.Scan() {
		m.Partial = true
		return
	}
	if !strings.HasPrefix(sc.Text(), "ply") {
		m.Partial = true
		m.Notes = append(m.Notes, "not a PLY file")
		return
	}
	var verts, faces int64
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "end_header" {
			break
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "element" {
			n, _ := strconv.ParseInt(fields[2], 10, 64)
			switch fields[1] {
			case "vertex":
				verts = n
			case "face":
				faces = n
			}
		}
	}
	m.VertCount = verts
	// Faces are polygons; assume average of 2 tris/quad boundary, use 1 tri per
	// face as the lower bound estimate.
	m.TriCount = faces
}

// STL (binary or ASCII).
func parseSTL(r io.Reader, m *Metadata) {
	buf, err := io.ReadAll(io.LimitReader(r, 96))
	if err != nil {
		m.Partial = true
		return
	}
	head := buf
	if len(head) >= 5 && strings.EqualFold(strings.TrimSpace(string(head[:5])), "solid") {
		// Possibly ASCII; count facets by scanning (bounded).
		rest, err := io.ReadAll(io.LimitReader(r, 128<<20))
		if err != nil {
			m.Partial = true
		}
		m.TriCount = int64(strings.Count(string(rest), "facet normal"))
		return
	}
	// Binary STL: 80-byte header + uint32 triangle count + 50 bytes/tri.
	if len(head) < 84 {
		m.Partial = true
		return
	}
	count := int64(head[80]) | int64(head[81])<<8 | int64(head[82])<<16 | int64(head[83])<<24
	m.TriCount = count
	m.VertCount = count * 3
}

// COLLADA (.dae) — very light scan for names and animation presence.
func parseDAE(r io.Reader, m *Metadata) {
	buf, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		m.Partial = true
		return
	}
	s := string(buf)
	m.TriCount = int64(strings.Count(s, "<triangles"))
	for _, id := range extractXMLAttrs(s, "<geometry ", "name") {
		addUnique(&m.MeshNames, id)
	}
	for _, id := range extractXMLAttrs(s, "<material ", "name") {
		addUnique(&m.MaterialNames, id)
	}
	for _, id := range extractXMLAttrs(s, "<image ", "name") {
		addUnique(&m.MeshNames, id)
	}
	if strings.Contains(s, "<library_animations") {
		m.AnimationNames = append(m.AnimationNames, "collada-animation")
	}
	if n := strings.Count(s, "<image "); n > 0 {
		m.TextureCount = n
	}
}

func extractXMLAttrs(s, marker, attr string) []string {
	var out []string
	rest := s
	for len(rest) > 0 {
		i := strings.Index(rest, marker)
		if i < 0 {
			break
		}
		rest = rest[i+len(marker):]
		j := strings.Index(rest, ">")
		if j < 0 {
			break
		}
		tag := rest[:j]
		key := attr + `="`
		if k := strings.Index(tag, key); k >= 0 {
			v := tag[k+len(key):]
			if e := strings.Index(v, `"`); e >= 0 {
				out = append(out, v[:e])
			}
		}
	}
	return out
}
