// Package naming derives search-friendly words from asset file names and
// paths: cleaned names ("SM_Prop_WallLantern_01" -> "Prop Wall Lantern"),
// library pack names and the keys used to spot duplicate exports.
package naming

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Leading pack/asset-kind codes used by Synty and similar libraries
// (SM_ static mesh, SK_ skinned, SF_ Simple Fantasy, ...) and the type
// abbreviations that follow them.
var (
	namePrefixes = map[string]bool{"sm": true, "sk": true, "sf": true, "si": true, "sr": true, "ai": true,
		"fx": true, "pf": true, "bp": true, "sh": true, "sp": true, "se": true}
	nameCodes = map[string]string{"veh": "vehicle", "bld": "building", "wep": "weapon", "env": "environment",
		"chr": "character", "fx": "effect", "gen": "", "sh": ""}
	camelBoundary   = regexp.MustCompile(`([a-z])([A-Z])`)
	acronymBoundary = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
)

// CleanName turns a file-derived title like "SF Veh BatteringRam 01" into
// "vehicle Battering Ram": drops pack prefixes and variant numbers, expands
// type codes and splits CamelCase.
func CleanName(title string) string {
	var out []string
	for i, t := range strings.Fields(title) {
		tl := strings.ToLower(t)
		if i == 0 && namePrefixes[tl] {
			continue
		}
		if _, err := strconv.Atoi(t); err == nil {
			continue
		}
		if exp, ok := nameCodes[tl]; ok {
			if exp != "" {
				out = append(out, exp)
			}
			continue
		}
		out = append(out, acronymBoundary.ReplaceAllString(camelBoundary.ReplaceAllString(t, "$1 $2"), "$1 $2"))
	}
	if len(out) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.Join(out, " ")
}

var (
	// A library pack directory: "POLYGON - Orchard Pack", "SIMPLE Canyon",
	// "Vendor Harbor Kit", "HarborProps_Pack" ...
	packDir = regexp.MustCompile(`(?i)^(polygon|simple|synty|kenney|quaternius)\b.*[a-z]{3}.*| - |(^|[ _-])(pack|kit)([ _-]|$)`)
	// Export plumbing between a model and its pack: format folders, source
	// trees, "Unsupported" legacy exports.
	exportDir = regexp.MustCompile(`(?i)^(fbx|obj|gltf|glb|blend|dae|stl|ply|models?|meshes|mesh|source|sources|source[ _-]?files?|unsupported|export(s|ed)?|staticmeshes|static[ _-]?meshes)$|_v\d+$`)
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	// Vendor prefixes and suffixes shared by every pack of a library.
	packNoise = regexp.MustCompile(`(?i)^(polygon|simple|synty)( mini)?\s*-?\s*|[ _-]*(pack|kit)$|\s*-\s*cartoon assets$`)
)

// PackKey identifies where an asset comes from: its library pack directory
// when the path has one, else its folder with export plumbing removed, so
// "kit/FBX/Chair_01.fbx" and "kit/OBJ/Chair_01.obj" share a key.
func PackKey(path string) string {
	segs := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for i, s := range segs {
		if packDir.MatchString(s) {
			return strings.Join(segs[:i+1], "/")
		}
	}
	for len(segs) > 1 && exportDir.MatchString(segs[len(segs)-1]) {
		segs = segs[:len(segs)-1]
	}
	return strings.Join(segs, "/")
}

// PackName is a readable pack label for the text document ("Orchard"
// for ".../POLYGON - Orchard Pack/..."), or "" when the path has no pack folder.
func PackName(path string) string {
	segs := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for _, s := range segs {
		if !packDir.MatchString(s) {
			continue
		}
		s = packNoise.ReplaceAllString(s, "")
		s = strings.TrimSpace(strings.ReplaceAll(s, "_", " "))
		return s
	}
	return ""
}

// DupName normalizes a title for duplicate grouping: case and separators
// do not matter, variant numbers do ("Chair 01" and "Chair 02" differ).
func DupName(title string) string { return nonAlnum.ReplaceAllString(strings.ToLower(title), "") }
