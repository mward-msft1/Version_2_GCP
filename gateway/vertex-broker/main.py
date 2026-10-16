"""OpenAI-compatible broker from Azure AI Gateway to Vertex AI.

Vertex's OpenAI endpoint accepts only Google OAuth principals. AI Gateway
custom providers can store a static header, so this service checks that
header and calls Vertex with the Cloud Run service account token.
"""

import json
import os
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROJECT = os.environ["VERTEX_PROJECT"]
LOCATION = os.environ.get("VERTEX_LOCATION", "us-central1")
SECRET = os.environ["BROKER_KEY"]
VERTEX = (
    f"https://aiplatform.googleapis.com/v1/projects/{PROJECT}"
    f"/locations/{LOCATION}/endpoints/openapi/chat/completions"
)
MODELS = {
    "gemini-2.5-flash": "google/gemini-2.5-flash",
    "gemini-2-5-flash": "google/gemini-2.5-flash",
    "google/gemini-2.5-flash": "google/gemini-2.5-flash",
    "gemini-flash-latest": "google/gemini-2.5-flash",
}


def google_token():
    req = urllib.request.Request(
        "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token",
        headers={"Metadata-Flavor": "Google"},
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        return json.load(resp)["access_token"]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _authorized(self):
        presented = self.headers.get("x-api-key") or ""
        auth = self.headers.get("Authorization") or ""
        if auth.lower().startswith("bearer "):
            presented = presented or auth[7:].strip()
        return presented == SECRET

    def _send(self, code, body, content_type="application/json"):
        if isinstance(body, str):
            body = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/").endswith("/healthz"):
            return self._send(200, {"status": "ok"})
        if not self._authorized():
            return self._send(401, {"error": {"message": "unauthorized"}})
        if self.path.rstrip("/").endswith("/models"):
            return self._send(
                200,
                {"object": "list", "data": [{"id": "gemini-2.5-flash", "object": "model"}]},
            )
        self._send(404, {"error": {"message": "not found"}})

    def do_POST(self):
        if not self._authorized():
            return self._send(401, {"error": {"message": "unauthorized"}})
        if not self.path.rstrip("/").endswith("/chat/completions"):
            return self._send(404, {"error": {"message": "not found"}})
        length = int(self.headers.get("Content-Length") or 0)
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return self._send(400, {"error": {"message": "invalid JSON"}})
        model = str(payload.get("model") or "gemini-2.5-flash")
        payload["model"] = MODELS.get(model, model if model.startswith("google/") else f"google/{model}")
        token = google_token()
        req = urllib.request.Request(
            VERTEX,
            data=json.dumps(payload).encode(),
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=90) as resp:
                content_type = resp.headers.get("Content-Type", "application/json")
                self._send(resp.status, resp.read(), content_type)
        except urllib.error.HTTPError as exc:
            content_type = exc.headers.get("Content-Type", "application/json") if exc.headers else "application/json"
            self._send(exc.code, exc.read() or b"{}", content_type)
        except Exception as exc:
            self._send(502, {"error": {"message": str(exc)[:300]}})

    def log_message(self, fmt, *args):
        return


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8080"))
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
