# Texture matching tests for render_preview.py; run outside Blender:
#   python3 -m unittest internal/preview/render_preview_test.py
import importlib.util
import os
import sys
import tempfile
import types
import unittest

for _m in ("bpy", "mathutils"):
    _stub = types.ModuleType(_m)
    _stub.Vector = object
    sys.modules.setdefault(_m, _stub)
_spec = importlib.util.spec_from_file_location(
    "render_preview", os.path.join(os.path.dirname(os.path.abspath(__file__)), "render_preview.py"))
rp = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(rp)


def touch(path):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    open(path, "w").close()


class LocateTexture(unittest.TestCase):
    def setUp(self):
        rp._dir_index.clear()
        rp._image_children.clear()
        rp._key_index.clear()
        self.tmp = tempfile.TemporaryDirectory()
        lib = self.tmp.name
        # A library of packs, like ~/Assets/Synty.
        for pack, files in {
            "Town": ["PolygonTown_01_A.png", "PolygonTown_01_B.png", "PolygonTown_Normals.png", "Road_01.png"],
            "Royale": ["PolygonBattleRoyale_Texture_01_A.png", "PolygonBattleRoyale_Vehicles_01.png",
                       "PolygonBattleRoyale_Vehicles_01_Damaged.png", "PolygonBattleRoyale_Vehicles_01_Emmision.png"],
            "War": ["PolygonWar_Texture_01_A.png", "PolygonWar_Texture_01_B.png", "Rubble.png"],
            "Zombies": ["PolygonZombie_Texture_01_A.png", "PolygonZombie_Emissive.png"],
            "Generic": ["Generic_01_A.png", "Generic_02_A.png"],
            "City": ["SimpleCity_Texture.png"],
        }.items():
            for f in files:
                touch(os.path.join(lib, pack, "Source", "Textures", f))
            touch(os.path.join(lib, pack, "Source", "FBX", "Characters", "SM_Thing.fbx"))
        self.lib = lib

    def tearDown(self):
        self.tmp.cleanup()

    def roots(self, pack, nested=False):
        d = os.path.join(self.lib, pack, "Source", "FBX")
        if nested:
            d = os.path.join(d, "Characters")
        out = []
        for _ in range(4):
            out.append(d)
            d = os.path.dirname(d)
        return out

    def find(self, pack, name, nested=False):
        hit, _ = rp.locate_texture(self.roots(pack, nested), name.lower())
        return os.path.basename(hit) if hit else None

    def test_renamed_and_working_copies(self):
        cases = [
            ("Town", "PolygonTown_Texture_01_A.psd", "PolygonTown_01_A.png"),
            ("Town", "PolygonTown_Texture_01_Andrew.psd", "PolygonTown_01_A.png"),
            ("Zombies", "PolygonZombie_Texture_A.psd", "PolygonZombie_Texture_01_A.png"),
            ("Royale", "PolygonMilitary_Texture_01_A.psd", "PolygonBattleRoyale_Texture_01_A.png"),
            ("Royale", "Land_Vehicle_Master_Destroyed_01.psd", "PolygonBattleRoyale_Vehicles_01.png"),
            ("War", "PolygonWar_Texture_GoodVersion.psd", "PolygonWar_Texture_01_A.png"),
            ("War", "rubble_02.psd", "Rubble.png"),
            ("Generic", "PolygonGeneric_Texture_01_A.psd", "Generic_01_A.png"),
            ("City", "City_Texture.psd", "SimpleCity_Texture.png"),
        ]
        for pack, name, want in cases:
            with self.subTest(name=name):
                self.assertEqual(self.find(pack, name), want)

    def test_nested_asset_still_finds_its_pack(self):
        self.assertEqual(self.find("Zombies", "PolygonZombie_Texture_A.psd", nested=True), "PolygonZombie_Texture_01_A.png")

    def test_never_borrows_another_packs_atlas(self):
        # The library root holds every pack; fuzzy tiers must not search it.
        self.assertTrue(rp.is_library(self.lib))
        self.assertIsNone(self.find("City", "PolygonMilitary_Texture_01_A.psd"))

    def test_missing_non_colour_maps_are_not_guessed(self):
        self.assertIsNone(self.find("Town", "PolygonTown_Texture_Normal_A.png"))
        self.assertIsNone(self.find("Royale", "Building_Emmission_01.psd"))


if __name__ == "__main__":
    unittest.main()
