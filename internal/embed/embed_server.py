# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "sentence-transformers>=6.1.0",
#   "transformers>=5.19",
#   "torch",
#   "torchvision",
#   "pillow",
# ]
# ///
"""modon embedding sidecar for EmbeddingGemma 2 (text + vision).

Speaks the OpenAI /v1/embeddings shape so any compatible server works for
text, plus one extension modon uses for images: an input item may be an
object {"text": "...", "image": "<path | http(s) URL | data: URL | base64>"}.

Text arrives already prefixed by modon ("task: search result | query: ...",
"title: ... | text: ..."), so the server encodes it verbatim. Images carry no
prefix, per the model card.

Run:  uv run embed_server.py [--host 127.0.0.1] [--port 8178] [--image-tokens 280]
modon launches it itself with `modon embed-server`.
"""

import argparse
import base64
import io
import json
import sys
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL_ID = "google/embeddinggemma-2"
MAX_IMAGE_BYTES = 32 << 20


def log(*a):
    print("[modon-embed]", *a, file=sys.stderr, flush=True)


def load_image(ref):
    from PIL import Image

    if ref.startswith("data:"):
        raw = base64.b64decode(ref.split(",", 1)[1])
    elif ref.startswith(("http://", "https://")):
        req = urllib.request.Request(ref, headers={"User-Agent": "modon-embed/1"})
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read(MAX_IMAGE_BYTES)
    elif len(ref) > 512 and "/" not in ref[:64]:
        raw = base64.b64decode(ref)  # bare base64 payload
    else:
        with open(ref, "rb") as f:
            raw = f.read(MAX_IMAGE_BYTES)
    img = Image.open(io.BytesIO(raw))
    img.load()
    if img.mode in ("RGBA", "LA", "P"):
        # Thumbnails are often transparent; flatten onto mid-grey so the
        # silhouette is visible instead of black-on-black.
        img = img.convert("RGBA")
        bg = Image.new("RGBA", img.size, (128, 128, 128, 255))
        bg.alpha_composite(img)
        img = bg
    return img.convert("RGB")


class Embedder:
    def __init__(self, vision=True, image_tokens=None, device=None):
        import torch
        from sentence_transformers import SentenceTransformer

        cfg = {"audio_config": None}
        if not vision:
            cfg["vision_config"] = None

        def load(dev):
            bf16 = dev == "cuda" and torch.cuda.is_bf16_supported()
            dtype = torch.bfloat16 if bf16 else torch.float32  # never float16
            log(f"loading {MODEL_ID} on {dev} ({dtype}), vision={vision}")
            m = SentenceTransformer(
                MODEL_ID,
                device=dev,
                model_kwargs={"torch_dtype": dtype},
                config_kwargs=cfg,
            )
            return m, dtype

        auto = device is None
        if auto:
            device = "cuda" if torch.cuda.is_available() else "cpu"
        try:
            self.model, dtype = load(device)
        except torch.OutOfMemoryError:
            if not auto or device == "cpu":
                raise
            log("CUDA out of memory (GPU busy?), falling back to CPU")
            torch.cuda.empty_cache()
            device = "cpu"
            self.model, dtype = load(device)
        if vision and image_tokens:
            self._set_image_tokens(image_tokens)
        self.vision = vision
        self.device = device
        self.dtype = str(dtype).replace("torch.", "")
        self.dims = self.model.get_sentence_embedding_dimension() or 768
        self.lock = threading.Lock()
        log("ready")

    def _set_image_tokens(self, n):
        proc = getattr(self.model[0], "processor", None)
        ip = getattr(proc, "image_processor", None)
        if ip is None:
            log("cannot set image token budget: no image processor")
            return
        for attr in ("max_soft_tokens", "image_seq_length"):
            if hasattr(ip, attr):
                setattr(ip, attr, n)
        if hasattr(proc, "image_seq_length"):
            proc.image_seq_length = n
        log(f"image token budget = {n}")

    def encode(self, items, dims=None, batch_size=16):
        texts, mm = [], []
        for i, it in enumerate(items):
            if isinstance(it, str):
                texts.append((i, it))
            elif isinstance(it, dict) and it.get("image"):
                if not self.vision:
                    raise ValueError("server started without the vision encoder")
                text = (it.get("text") or "").strip()
                img = load_image(it["image"])
                inp = {"image": [img]}
                inp["text"] = (text + " <|image|>").strip() if text else "<|image|>"
                mm.append((i, inp))
            elif isinstance(it, dict):
                texts.append((i, it.get("text") or ""))
            else:
                raise ValueError(f"input {i}: expected string or object")

        out = [None] * len(items)
        kw = {"normalize_embeddings": True, "prompt": ""}
        if dims:
            kw["truncate_dim"] = dims
        with self.lock:
            if texts:
                vecs = self.model.encode([t for _, t in texts], batch_size=batch_size, **kw)
                for (i, _), v in zip(texts, vecs):
                    out[i] = v
            if mm:
                vecs = self.model.encode([m for _, m in mm], batch_size=max(1, batch_size // 2), **kw)
                for (i, _), v in zip(mm, vecs):
                    out[i] = v
        return [[float(x) for x in v] for v in out]


def make_handler(emb):
    class H(BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def _json(self, code, obj):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path in ("/modon/info", "/health"):
                self._json(200, {
                    "model": MODEL_ID,
                    "vision": emb.vision,
                    "dims": emb.dims,
                    "device": emb.device,
                    "dtype": emb.dtype,
                })
            elif self.path == "/v1/models":
                self._json(200, {"object": "list", "data": [{"id": MODEL_ID, "object": "model"}]})
            else:
                self._json(404, {"error": "not found"})

        def do_POST(self):
            if self.path not in ("/v1/embeddings", "/embeddings"):
                self._json(404, {"error": "not found"})
                return
            try:
                n = int(self.headers.get("Content-Length") or 0)
                req = json.loads(self.rfile.read(n) or b"{}")
                inp = req.get("input")
                if isinstance(inp, (str, dict)):
                    inp = [inp]
                if not isinstance(inp, list) or not inp:
                    raise ValueError("input must be a non-empty list")
                vecs = emb.encode(inp, dims=req.get("dimensions"))
            except Exception as e:  # report, keep serving
                log("error:", repr(e))
                self._json(400, {"error": {"message": str(e)}})
                return
            self._json(200, {
                "object": "list",
                "model": MODEL_ID,
                "data": [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vecs)],
                "usage": {"prompt_tokens": 0, "total_tokens": 0},
            })

    return H


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8178)
    ap.add_argument("--no-vision", action="store_true", help="text-only (270M) configuration")
    ap.add_argument("--image-tokens", type=int, default=0, help="vision token budget per image (70-1120, default model's 280)")
    ap.add_argument("--device", default=None, help="cuda | cpu | mps (default: auto)")
    ap.add_argument("--check", action="store_true", help="load the model, embed a probe, exit")
    a = ap.parse_args()

    emb = Embedder(vision=not a.no_vision, image_tokens=a.image_tokens or None, device=a.device)
    if a.check:
        v = emb.encode(["task: search result | query: wooden chair"], dims=256)[0]
        log(f"probe ok: {len(v)} dims")
        return
    srv = ThreadingHTTPServer((a.host, a.port), make_handler(emb))
    log(f"listening on http://{a.host}:{a.port}")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
