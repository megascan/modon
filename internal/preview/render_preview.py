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


def index_dir(root):
    if root not in _dir_index:
        stems = {}
        count = 0
        for dirpath, dirnames, files in os.walk(root):
            dirnames[:] = sorted(d for d in dirnames if not d.startswith("."))
            for fn in sorted(files):
                count += 1
                stem, ext = os.path.splitext(fn)
                if ext.lower() in IMAGE_EXTS:
                    stems.setdefault(stem.lower(), []).append(os.path.join(dirpath, fn))
            if count > 200000:  # do not crawl a whole drive
                break
        _dir_index[root] = stems
    return _dir_index[root]


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
    for img in missing:
        name = os.path.basename(bpy.path.abspath(img.filepath).replace("\\", "/")).lower()
        hit = None
        if name:
            for root in roots:
                hit = find_texture(index_dir(root), name)
                if hit:
                    break
        if hit:
            img.filepath = hit
            try:
                img.reload()
            except RuntimeError:
                unresolved += 1
        else:
            unresolved += 1
    return unresolved


def render(out, size, asset=None):
    scene = bpy.context.scene
    scene.render.engine = "BLENDER_WORKBENCH"
    unresolved = resolve_textures(asset) if asset else 0
    try:
        shading = scene.display.shading
        shading.light = "STUDIO"
        # Missing textures render magenta; material colours look saner.
        shading.color_type = "MATERIAL" if unresolved else "TEXTURE"
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
