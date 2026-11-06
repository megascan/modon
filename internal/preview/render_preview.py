# Renders a turntable-ish thumbnail of a mesh file with Workbench.
# Usage: blender --background --python render_preview.py -- <asset> <output.png> <size>
#        blender --background --python render_preview.py -- --batch <jobs.json> <size>
# The asset is imported by extension (.blend files are opened directly).
# Batch mode renders [[asset, output], ...] in one Blender session, resetting
# to an empty scene between jobs (Blender startup dominates single renders),
# and keeps going after per-asset errors. Every job prints
# MODON_PREVIEW_START:<i> then MODON_PREVIEW_DONE:<out> or
# MODON_PREVIEW_ERROR:<i>:<msg>, so a crash pinpoints the culprit.
# Single mode exits non-zero with an error on stderr when the format is
# unsupported.
import json
import os
import re
import sys
import traceback

import bpy
from mathutils import Vector


class PreviewError(Exception):
    pass


def empty_scene():
    # Factory-empty file: no default Cube/Light/Camera, and data from the
    # previous batch job is released.
    bpy.ops.wm.read_homefile(use_empty=True, use_factory_startup=True)


def load(asset):
    ext = os.path.splitext(asset)[1].lower()
    if ext == ".blend":
        bpy.ops.wm.open_mainfile(filepath=asset)
        return
    empty_scene()
    if ext in (".gltf", ".glb"):
        bpy.ops.import_scene.gltf(filepath=asset)
    elif ext == ".obj":
        bpy.ops.wm.obj_import(filepath=asset)
    elif ext == ".fbx":
        bpy.ops.import_scene.fbx(filepath=asset)
    elif ext == ".stl":
        bpy.ops.wm.stl_import(filepath=asset)
    elif ext == ".ply":
        bpy.ops.wm.ply_import(filepath=asset)
    elif ext == ".dae":
        bpy.ops.wm.collada_import(filepath=asset)
    elif ext in (".usd", ".usda", ".usdc", ".usdz"):
        bpy.ops.wm.usd_import(filepath=asset)
    elif ext == ".abc":
        bpy.ops.wm.alembic_import(filepath=asset)
    else:
        raise PreviewError("unsupported format %r (no Blender importer)" % ext)


IMAGE_EXTS = {".png", ".jpg", ".jpeg", ".tga", ".tif", ".tiff", ".bmp", ".exr", ".webp", ".psd", ".dds"}
_dir_index = {}  # directory -> {lowercase stem: [paths]}, kept across batch jobs
# Suffixes of non-colour maps that must not stand in for a missing albedo.
NON_COLOR = ("normal", "_n", "emission", "emissive", "metal", "rough", "spec", "_ao", "occlusion",
             "mask", "height", "_disp", "opacity", "alpha", "gloss")


_image_children = {}  # directory -> top-level subfolders holding images


def index_dir(root):
    if root not in _dir_index:
        stems = {}
        children = set()
        count = 0
        for dirpath, dirnames, files in os.walk(root):
            dirnames[:] = sorted(d for d in dirnames if not d.startswith("."))
            for fn in sorted(files):
                count += 1
                stem, ext = os.path.splitext(fn)
                if ext.lower() in IMAGE_EXTS:
                    stems.setdefault(stem.lower(), []).append(os.path.join(dirpath, fn))
                    rel = os.path.relpath(dirpath, root)
                    children.add(rel.split(os.sep)[0] if rel != "." else ".")
            if count > 200000:  # do not crawl a whole drive
                break
        _dir_index[root] = stems
        _image_children[root] = children
    return _dir_index[root]


def is_library(root):
    """A folder whose images sit in three or more separate subfolders is a
    collection of packs (e.g. ~/Assets/Synty), not one pack."""
    index_dir(root)
    return len(_image_children[root] - {"."}) >= 3


def find_texture(stems, name):
    """Find a stand-in for a referenced texture: same stem with any image
    extension (packs ship PNGs for the artist's .psd), else a variant of
    it (Texture_01 -> Texture_01_A), preferring the primary _A variant."""
    stem = os.path.splitext(name)[0].lower()
    if not stem:
        return None
    if stem in stems:
        return stems[stem][0]
    variants = [s for s in stems if s.startswith(stem + "_") and not any(t in s[len(stem):] for t in NON_COLOR)]
    if not variants:
        return None
    variants.sort(key=lambda s: (s != stem + "_a", len(s), s))
    return stems[variants[0]][0]


# Fuzzy matching for packs whose FBX files name textures differently from
# the files shipped (PolygonTown_Texture_01_A -> PolygonTown_01_A.png,
# PolygonZombie_Texture_A -> PolygonZombie_Texture_01_A.png, an older pack
# name like PolygonMilitary_ for PolygonBattleRoyale_, working-copy suffixes
# like _GoodVersion or _Andrew).
_TOKEN = re.compile(r"[^a-z0-9]+")
_FILLER = {"texture", "textures", "tex", "01", "1", "001"}
_PACK_PREFIXES = ("polygon", "simple", "synty")
_NON_COLOR_PREFIXES = ("normal", "nrml", "emiss", "emmis", "emision", "metal", "rough", "spec", "gloss",
                       "occlusion", "height", "disp", "opacity", "alpha", "mask")
_NON_COLOR_WORDS = {"n", "ao"}
_key_index = {}  # (root, drop_pack) -> {key: [stems]}


def _tokens(stem):
    return [t for t in _TOKEN.split(stem.lower()) if t]


def _non_color(stem):
    return any(t in _NON_COLOR_WORDS or t.startswith(_NON_COLOR_PREFIXES) for t in _tokens(stem))


def _key(stem, drop_pack):
    """Normalized name: no filler words, and a leading pack prefix glued to
    the first word is removed (PolygonGeneric -> generic, SimpleCity ->
    city). drop_pack drops that first word entirely (PolygonMilitary_ vs
    PolygonBattleRoyale_ naming the same atlas)."""
    toks = [t for t in _tokens(stem) if t not in _FILLER]
    if toks and toks[0].startswith(_PACK_PREFIXES):
        if drop_pack:
            toks = toks[1:]
        else:
            rest = toks[0]
            for p in _PACK_PREFIXES:
                if rest.startswith(p):
                    rest = rest[len(p):]
                    break
            toks = ([rest] if rest else []) + toks[1:]
    return "_".join(toks)


def _keys(root, drop_pack):
    k = (root, drop_pack)
    if k not in _key_index:
        idx = {}
        for s in index_dir(root):
            if not _non_color(s):
                idx.setdefault(_key(s, drop_pack), []).append(s)
        _key_index[k] = idx
    return _key_index[k]


def _by_key(root, stem, drop_pack):
    """Colour texture whose normalized name equals stem's, else its primary
    variant (key + "_a" first)."""
    want = _key(stem, drop_pack)
    idx = _keys(root, drop_pack)
    if not want:
        # Only pack name + filler (Polygon_Texture_01): the pack's main atlas.
        if not drop_pack:
            return None
        want = "a"
    hits = idx.get(want)
    if not hits:
        variants = [k for k in idx if k.startswith(want + "_")]
        if not variants and drop_pack and any(c.isdigit() for c in want):
            # Same sheet number under another pack name: ..._Texture_02_A.
            variants = [k for k in idx if k.endswith("_" + want)]
        if not variants:
            return None
        variants.sort(key=lambda k: (not k.endswith("_a"), len(k), k))
        hits = idx[variants[0]]
    best = min(hits, key=lambda s: (len(s), s))
    return index_dir(root)[best][0]


def _singular(t):
    return t[:-1] if len(t) > 4 and t.endswith("s") else t


def _by_overlap(root, stem):
    """Last resort: the colour texture sharing the most name words
    (Land_Vehicle_Master_Destroyed_01 -> ..._Vehicles_01.png)."""
    want = {_singular(t) for t in _key(stem, True).split("_") if len(t) >= 4 and not t.isdigit()}
    if not want:
        return None
    best, best_rank = None, None
    for s in index_dir(root):
        if _non_color(s):
            continue
        have = {_singular(t) for t in _key(s, True).split("_") if t}
        common = len(want & have)
        if common == 0:
            continue
        rank = (-common, len(have - want), len(s), s)
        if best_rank is None or rank < best_rank:
            best, best_rank = s, rank
    return index_dir(root)[best][0] if best else None


def locate_texture(roots, name):
    """Resolve a referenced texture file name against the asset's folder and
    its parents. Returns (path, guessed): exact and variant matches may use
    every root; the fuzzy tiers (guessed=True) skip folders that hold several
    packs, so they stay inside the asset's own pack."""
    stem = os.path.splitext(name)[0].lower()
    if not stem:
        return None, False
    for root in roots:
        hit = find_texture(index_dir(root), name)
        if hit:
            return hit, False
    if _non_color(stem):
        return None, False  # a missing normal/emission map is dropped, never guessed
    return _guess_texture([r for r in roots if not is_library(r)], roots, stem), True


def _guess_texture(near, roots, stem):
    toks = _tokens(stem)
    # Drop trailing words one at a time: Texture_GoodVersion, rubble_02.
    for n in range(len(toks), 0, -1):
        part = "_".join(toks[:n])
        if n < len(toks) and not _key(part, False):
            break  # only filler words left
        for root in roots if n < len(toks) else ():
            hit = find_texture(index_dir(root), part)
            if hit:
                return hit
        for drop_pack in (False, True):
            for root in near:
                hit = _by_key(root, part, drop_pack)
                if hit:
                    return hit
    for root in near:
        hit = _by_overlap(root, stem)
        if hit:
            return hit
    return None


def image_missing(img):
    if img.source not in {"FILE", "SEQUENCE", "TILED"} or img.packed_file:
        return False
    p = bpy.path.abspath(img.filepath, library=img.library)
    return not p or not os.path.exists(p)


def resolve_textures(asset):
    """Relink textures whose stored paths point elsewhere (asset packs are
    usually exported from the artist's machine) by file name, searching the
    asset's folder and up to three parents. Returns how many stay missing."""
    missing = [img for img in bpy.data.images if image_missing(img)]
    if not missing:
        return 0
    roots = []
    d = os.path.dirname(os.path.abspath(asset))
    for _ in range(4):
        roots.append(d)
        parent = os.path.dirname(d)
        if parent == d:
            break
        d = parent
    unresolved = 0
    guessed = set()
    for img in missing:
        name = os.path.basename(bpy.path.abspath(img.filepath).replace("\\", "/")).lower()
        hit, guess = locate_texture(roots, name) if name else (None, False)
        if hit and guess:
            guessed.add(img)
        if hit:
            img.filepath = hit
            try:
                img.reload()
            except RuntimeError:
                unresolved += 1
        else:
            unresolved += 1
    if guessed:
        unresolved += reject_tiling_guesses(guessed)
    return unresolved


def reject_tiling_guesses(guessed):
    """A guessed atlas is wrong for a material made for a tiling texture: it
    would show the atlas sheet itself. Atlas-mapped meshes put every face on
    a small swatch; tiling ones (floors, wall strips) have faces spanning
    most of the texture in at least one direction. Such guesses are dropped
    again (material colour). Returns how many."""
    tiling = set()
    for ob in bpy.data.objects:
        me = ob.data if ob.type == "MESH" else None
        if me is None or not me.uv_layers or not me.materials:
            continue
        uv = me.uv_layers.active.data
        for poly in me.polygons:
            if poly.material_index >= len(me.materials):
                continue
            mat = me.materials[poly.material_index]
            if mat is None or mat in tiling:
                continue
            us = [uv[li].uv[0] for li in poly.loop_indices]
            vs = [uv[li].uv[1] for li in poly.loop_indices]
            du, dv = max(us) - min(us), max(vs) - min(vs)
            if du > 0.6 or dv > 0.6:
                tiling.add(mat)
    dropped = set()
    for mat in tiling:
        if not mat.node_tree:
            continue
        for node in mat.node_tree.nodes:
            if node.type == "TEX_IMAGE" and node.image in guessed:
                dropped.add(node.image)
                node.image = None
    return len(dropped)


def drop_missing_images():
    """Detach image nodes whose file is still missing, so Workbench draws
    those materials in their material colour rather than magenta while the
    rest of the asset keeps its textures (one unresolved map, often just a
    normal map, used to turn the whole thumbnail flat grey)."""
    for mat in bpy.data.materials:
        if not mat.use_nodes or mat.node_tree is None:
            continue
        for node in mat.node_tree.nodes:
            if node.type == "TEX_IMAGE" and node.image is not None and image_missing(node.image):
                node.image = None


def render(out, size, asset=None):
    scene = bpy.context.scene
    scene.render.engine = "BLENDER_WORKBENCH"
    if asset:
        resolve_textures(asset)
        drop_missing_images()
    try:
        shading = scene.display.shading
        shading.light = "STUDIO"
        # Materials whose image is still missing had it removed above, so
        # they show their material colour instead of magenta.
        shading.color_type = "TEXTURE"
        shading.show_object_outline = True
        shading.show_cavity = True
        shading.cavity_type = "SCREEN"
    except AttributeError:
        pass
    scene.display.render_aa = "8"
    scene.render.resolution_x = size
    scene.render.resolution_y = size
    scene.render.resolution_percentage = 100
    scene.render.film_transparent = False
    scene.render.image_settings.file_format = "PNG"

    if scene.world is None:
        scene.world = bpy.data.worlds.new("PreviewWorld")

    meshes = [o for o in scene.objects if o.type == "MESH"]
    if meshes:
        corners = [
            o.matrix_world @ Vector(c)
            for o in meshes
            for c in o.bound_box
        ]
        mn = Vector((min(v.x for v in corners), min(v.y for v in corners), min(v.z for v in corners)))
        mx = Vector((max(v.x for v in corners), max(v.y for v in corners), max(v.z for v in corners)))
        center = (mn + mx) / 2
        diag = max((mx - mn).length, 1e-3)

        cam_data = bpy.data.cameras.new("PreviewCam")
        cam = bpy.data.objects.new("PreviewCam", cam_data)
        scene.collection.objects.link(cam)
        scene.camera = cam
        d = diag * 1.6
        cam.location = center + Vector((d * 0.75, -d * 0.85, d * 0.5))
        direction = center - cam.location
        cam.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
        cam_data.clip_start = max(diag / 1000.0, 0.001)
        cam_data.clip_end = max(diag * 20.0, 100.0)
    else:
        # Animation-only or empty file: render a neutral frame.
        bpy.ops.mesh.primitive_cube_add(size=1)
        if scene.camera is None:
            cam = bpy.data.objects.new("PreviewCam", bpy.data.cameras.new("PreviewCam"))
            scene.collection.objects.link(cam)
            scene.camera = cam
            cam.location = Vector((2.2, -2.5, 1.6))
            cam.rotation_euler = (-cam.location).to_track_quat("-Z", "Y").to_euler()

    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    scene.render.filepath = out
    bpy.ops.render.render(write_still=True)


def one_line(exc):
    lines = traceback.format_exception_only(type(exc), exc)
    return (lines[-1] if lines else repr(exc)).strip().replace("\n", " ")


def main():
    argv = sys.argv[sys.argv.index("--") + 1:]

    if argv[0] == "--batch":
        with open(argv[1]) as f:
            jobs = json.load(f)
        size = int(argv[2])
        for i, (asset, out) in enumerate(jobs):
            print("MODON_PREVIEW_START:%d" % i, flush=True)
            try:
                load(asset)
                render(out, size, asset)
                print("MODON_PREVIEW_DONE:" + out, flush=True)
            except Exception as e:  # keep the batch going
                print("MODON_PREVIEW_ERROR:%d:%s" % (i, one_line(e)), flush=True)
        sys.exit(0)

    asset, out, size = argv[0], argv[1], int(argv[2])
    try:
        load(asset)
    except PreviewError as e:
        print("MODON_PREVIEW_ERROR:" + str(e), file=sys.stderr)
        sys.exit(1)
    except Exception as e:
        print("MODON_PREVIEW_ERROR:" + one_line(e), file=sys.stderr)
        sys.exit(1)
    render(out, size, asset)
    print("MODON_PREVIEW_DONE:" + out)


if __name__ == "__main__":
    main()
