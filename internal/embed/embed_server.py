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
                             [--idle-offload 600]
modon launches it itself with `modon embed-server` (or the systemd user unit
from `modon service install`).

Errors: 400 for bad input (do not retry), 503 when the GPU stayed out of
memory (retry later or with fewer inputs), 500 otherwise.
"""

import argparse
import base64
import io
import json
import sys
import threading
import time
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


def is_oom(e):
    import torch

    if isinstance(e, torch.OutOfMemoryError):
        return True
    msg = str(e).lower()
    return "out of memory" in msg and ("cuda" in msg or "cublas" in msg)


class Embedder:
    """Holds the model and moves it between GPU and CPU.

    The GPU is usually shared with an editor or a game, so the server is
    conservative: CUDA out-of-memory during encode halves the batch, then
    runs that request on CPU and stays off the GPU for a cool-down; an idle
    model is moved off the GPU after --idle-offload seconds and comes back
    on the next request when enough memory is free.
    """

    CPU_COOLDOWN = 120  # seconds on CPU after an OOM before trying the GPU again
    GPU_HEADROOM = 768 << 20  # free VRAM required beyond the weights to move back

    def __init__(self, vision=True, image_tokens=None, device=None, idle_offload=0):
        import torch
        from sentence_transformers import SentenceTransformer

        self.torch = torch
        cfg = {"audio_config": None}
        if not vision:
            cfg["vision_config"] = None

        def load(dev):
            dtype = self._dtype_for(dev)
            log(f"loading {MODEL_ID} on {dev} ({dtype}), vision={vision}")
            return SentenceTransformer(
                MODEL_ID,
                device=dev,
                model_kwargs={"torch_dtype": dtype},
                config_kwargs=cfg,
            )

        auto = device is None
        if auto:
            device = "cuda" if torch.cuda.is_available() else "cpu"
        # The device we want to run on; the model may sit elsewhere for a
        # while (OOM cool-down, idle offload).
        self.preferred = device
        self.cpu_until = 0.0
        self.oom_events = 0
        try:
            self.model = load(device)
        except torch.OutOfMemoryError:
            if not auto or device == "cpu":
                raise
            log("CUDA out of memory at load (GPU busy?), starting on CPU")
            torch.cuda.empty_cache()
            device = "cpu"
            self.cpu_until = time.time() + self.CPU_COOLDOWN
            self.model = load(device)
        if vision and image_tokens:
            self._set_image_tokens(image_tokens)
        self.vision = vision
        self.device = device
        self.dims = (getattr(self.model, "get_embedding_dimension", None) or self.model.get_sentence_embedding_dimension)() or 768
        self.weight_bytes = sum(p.numel() for p in self.model.parameters()) * 2  # bf16 on GPU
        self.lock = threading.Lock()
        self.last_used = time.time()
        self.idle_offload = idle_offload
        if idle_offload > 0 and self.preferred == "cuda":
            threading.Thread(target=self._idle_loop, daemon=True).start()
        log("ready")

    @property
    def dtype(self):
        return str(next(self.model.parameters()).dtype).replace("torch.", "")

    def _dtype_for(self, dev):
        torch = self.torch
        if dev == "cuda" and torch.cuda.is_bf16_supported():
            return torch.bfloat16
        return torch.float32  # never float16 (NaNs)

    def _free_cuda(self):
        if self.torch.cuda.is_available():
            self.torch.cuda.empty_cache()

    def _move(self, dev):
        """Move the model; caller holds the lock."""
        if dev == self.device:
            return
        self.model.to(device=dev, dtype=self._dtype_for(dev))
        self.device = dev
        if dev == "cpu":
            self._free_cuda()

    def _maybe_return_to_gpu(self):
        """Bring the model back to the preferred GPU when allowed and there is room."""
        if self.preferred != "cuda" or self.device == "cuda" or time.time() < self.cpu_until:
            return
        try:
            free, _ = self.torch.cuda.mem_get_info()
        except Exception:
            return
        if free < self.weight_bytes + self.GPU_HEADROOM:
            self.cpu_until = time.time() + 30  # check again shortly
            return
        try:
            self._move("cuda")
            log("model back on cuda")
        except Exception as e:
            if not is_oom(e):
                raise
            self.oom_events += 1
            log("CUDA out of memory moving back to the GPU, staying on CPU")
            self._move("cpu")
            self.cpu_until = time.time() + self.CPU_COOLDOWN

    def _idle_loop(self):
        while True:
            time.sleep(min(15, self.idle_offload))
            if self.device != "cuda" or time.time() - self.last_used < self.idle_offload:
                continue
            with self.lock:
                if self.device == "cuda" and time.time() - self.last_used >= self.idle_offload:
                    self._move("cpu")
                    log(f"idle for {self.idle_offload}s: moved off the GPU")

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

    def _encode_safe(self, inputs, batch_size, kw):
        """Encode, recovering from CUDA OOM: halve the batch down to 1, then
        run this request on CPU (and stay there for the cool-down)."""
        bs = batch_size
        while True:
            try:
                return self.model.encode(inputs, batch_size=bs, **kw)
            except Exception as e:
                if not is_oom(e) or self.device != "cuda":
                    raise
                self.oom_events += 1
                self._free_cuda()
                if bs > 1:
                    bs = max(1, bs // 2)
                    log(f"CUDA out of memory, retrying {len(inputs)} input(s) at batch size {bs}")
                    continue
                log("CUDA out of memory at batch size 1, running on CPU for a while")
                self._move("cpu")
                self.cpu_until = time.time() + self.CPU_COOLDOWN

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
            self.last_used = time.time()
            self._maybe_return_to_gpu()
            if texts:
                vecs = self._encode_safe([t for _, t in texts], batch_size, kw)
                for (i, _), v in zip(texts, vecs):
                    out[i] = v
            if mm:
                vecs = self._encode_safe([m for _, m in mm], max(1, batch_size // 2), kw)
                for (i, _), v in zip(mm, vecs):
                    out[i] = v
            self.last_used = time.time()
        return [[float(x) for x in v] for v in out]

    def info(self):
        return {
            "model": MODEL_ID,
            "vision": self.vision,
            "dims": self.dims,
            "device": self.device,
            "preferred_device": self.preferred,
            "dtype": self.dtype,
            "oom_events": self.oom_events,
            "idle_seconds": int(time.time() - self.last_used),
        }


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
                self._json(200, emb.info())
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
            except (ValueError, KeyError, TypeError, OSError) as e:  # bad input: do not retry
                log("bad request:", repr(e))
                self._json(400, {"error": {"message": str(e), "type": "invalid_request_error"}})
                return
            except Exception as e:  # report, keep serving; clients may retry
                transient = is_oom(e)
                log("error:", repr(e))
                self._json(503 if transient else 500, {"error": {
                    "message": str(e), "type": "out_of_memory" if transient else "server_error"}})
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
    ap.add_argument("--idle-offload", type=int, default=0,
                    help="move the model off the GPU after this many idle seconds (0 = never)")
    ap.add_argument("--check", action="store_true", help="load the model, embed a probe, exit")
    a = ap.parse_args()

    emb = Embedder(vision=not a.no_vision, image_tokens=a.image_tokens or None, device=a.device,
                   idle_offload=max(0, a.idle_offload))
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
