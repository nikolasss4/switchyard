"""A minimal OpenAI-compatible /v1/embeddings server around a sentence-transformers
model, so the semantic cache eval (and the gateway) can use a real embedder
locally. Usage: uv run --with sentence-transformers embed_server.py [model] [port]"""

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from sentence_transformers import SentenceTransformer

MODEL = sys.argv[1] if len(sys.argv) > 1 else "sentence-transformers/all-MiniLM-L6-v2"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8099
model = SentenceTransformer(MODEL)


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path.rstrip("/") not in ("/v1/embeddings", "/embeddings"):
            self.send_error(404)
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        inputs = body["input"] if isinstance(body["input"], list) else [body["input"]]
        vecs = model.encode(inputs, normalize_embeddings=True).tolist()
        out = {
            "object": "list",
            "model": MODEL,
            "data": [{"object": "embedding", "index": i, "embedding": v} for i, v in enumerate(vecs)],
            "usage": {"prompt_tokens": 0, "total_tokens": 0},
        }
        data = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


print(f"serving {MODEL} on :{PORT}", flush=True)
ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
