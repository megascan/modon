# Out-of-memory recovery tests for embed_server.py with a fake model; no
# torch or GPU needed:
#   python3 internal/embed/embed_server_test.py
import importlib.util
import json
import os
import sys
import threading
import types
import unittest
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer


class OutOfMemoryError(RuntimeError):
    pass


class FakeCuda:
    free = 6 << 30

    @staticmethod
    def is_available():
        return True

    @staticmethod
    def empty_cache():
        FakeCuda.emptied += 1

    @staticmethod
    def mem_get_info():
        return FakeCuda.free, 8 << 30

    @staticmethod
    def is_bf16_supported():
        return True

    emptied = 0


torch = types.ModuleType("torch")
torch.OutOfMemoryError = OutOfMemoryError
torch.cuda = FakeCuda
torch.bfloat16, torch.float32 = "bfloat16", "float32"
sys.modules.setdefault("torch", torch)
_spec = importlib.util.spec_from_file_location(
    "embed_server", os.path.join(os.path.dirname(os.path.abspath(__file__)), "embed_server.py"))
es = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(es)


class FakeModel:
    """Fails with CUDA OOM on GPU while the batch exceeds max_gpu_batch."""

    def __init__(self, max_gpu_batch):
        self.max_gpu_batch = max_gpu_batch
        self.device = "cuda"
        self.calls = []

    def encode(self, inputs, batch_size, **kw):
        self.calls.append((self.device, batch_size))
        if self.device == "cuda" and batch_size > self.max_gpu_batch:
            raise OutOfMemoryError("CUDA out of memory. Tried to allocate 60.00 MiB")
        return [[1.0, 0.0] for _ in inputs]

    def to(self, device=None, dtype=None):
        self.device = device
        return self


def embedder(model):
    e = es.Embedder.__new__(es.Embedder)
    e.torch = torch
    e.model = model
    e.device = "cuda"
    e.preferred = "cuda"
    e.vision = False
    e.dims = 2
    e.cpu_until = 0.0
    e.oom_events = 0
    e.weight_bytes = 1 << 30
    e.lock = threading.Lock()
    e.last_used = 0.0
    e.idle_offload = 0
    return e


class OOMRecovery(unittest.TestCase):
    def test_halves_batch_until_it_fits(self):
        m = FakeModel(max_gpu_batch=4)
        e = embedder(m)
        out = e.encode(["a"] * 10, batch_size=16)
        self.assertEqual(len(out), 10)
        self.assertEqual([b for _, b in m.calls], [16, 8, 4])
        self.assertEqual(e.device, "cuda")
        self.assertEqual(e.oom_events, 2)

    def test_falls_back_to_cpu_then_returns(self):
        m = FakeModel(max_gpu_batch=0)  # GPU full: even batch size 1 fails
        e = embedder(m)
        out = e.encode(["a", "b"], batch_size=2)
        self.assertEqual(len(out), 2)
        self.assertEqual(m.calls[-1], ("cpu", 1))
        self.assertEqual(e.device, "cpu")
        self.assertGreater(e.cpu_until, 0)
        # During the cool-down the model stays on CPU.
        e.encode(["c"])
        self.assertEqual(m.calls[-1][0], "cpu")
        # Afterwards, with room on the GPU, it moves back.
        e.cpu_until = 0
        m.max_gpu_batch = 64
        e.encode(["d"])
        self.assertEqual(m.calls[-1][0], "cuda")

    def test_stays_on_cpu_without_room(self):
        e = embedder(FakeModel(max_gpu_batch=64))
        e.device = "cpu"
        e.model.device = "cpu"
        FakeCuda.free = 256 << 20
        try:
            e.encode(["a"])
        finally:
            FakeCuda.free = 6 << 30
        self.assertEqual(e.model.calls[-1][0], "cpu")

    def test_other_errors_propagate(self):
        e = embedder(FakeModel(max_gpu_batch=64))
        e.model.encode = lambda *a, **k: (_ for _ in ()).throw(RuntimeError("shape mismatch"))
        with self.assertRaises(RuntimeError):
            e.encode(["a"])


class IdleExit(unittest.TestCase):
    def test_shuts_down_when_idle_only(self):
        e = embedder(FakeModel(max_gpu_batch=64))
        srv = ThreadingHTTPServer(("127.0.0.1", 0), es.make_handler(e))
        served = threading.Thread(target=srv.serve_forever, daemon=True)
        served.start()
        self.addCleanup(srv.server_close)
        e.last_used = es.time.time() + 3600  # busy: must keep serving
        watcher = threading.Thread(target=es.exit_when_idle, args=(srv, e, 1), daemon=True)
        watcher.start()
        watcher.join(1.5)
        self.assertTrue(served.is_alive())
        e.last_used = 0.0  # idle long enough
        watcher.join(5)
        served.join(5)
        self.assertFalse(served.is_alive())


class HTTPStatus(unittest.TestCase):
    def serve(self, emb):
        srv = ThreadingHTTPServer(("127.0.0.1", 0), es.make_handler(emb))
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.shutdown)
        return "http://127.0.0.1:%d" % srv.server_address[1]

    def post(self, url, body):
        req = urllib.request.Request(url + "/v1/embeddings", data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code

    def test_status_codes(self):
        class Emb:
            def __init__(self, exc):
                self.exc = exc

            def encode(self, inp, dims=None):
                raise self.exc

        self.assertEqual(self.post(self.serve(Emb(ValueError("bad"))), {"input": ["x"]}), 400)
        self.assertEqual(self.post(self.serve(Emb(OutOfMemoryError("CUDA out of memory"))), {"input": ["x"]}), 503)
        self.assertEqual(self.post(self.serve(Emb(RuntimeError("boom"))), {"input": ["x"]}), 500)


if __name__ == "__main__":
    unittest.main()
