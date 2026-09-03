# Renders a turntable-ish thumbnail of the open document with Workbench.
# Usage: blender -b <file> --python render_preview.py -- <output.png> <size>
import math
import sys

import bpy
from mathutils import Vector

argv = sys.argv[sys.argv.index("--") + 1:]
out, size = argv[0], int(argv[1])

scene = bpy.context.scene
scene.render.engine = "BLENDER_WORKBENCH"
try:
    shading = scene.display.shading
    shading.light = "STUDIO"
    shading.color_type = "TEXTURE"
    shading.show_object_outline = True
    shading.show_cavity = True
    shading.cavity_type = "SCREEN"
except AttributeError:
    pass
scene.display.render_aa = "8"
scene.render.resolution_x = size
scene.render.resolution_y = size
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

scene.render.filepath = out
bpy.ops.render.render(write_still=True)
print("MODON_PREVIEW_DONE:" + out)
