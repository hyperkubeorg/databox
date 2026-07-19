"""Hello-world databox layer: a stateless HTTP greeting service built on
databoxpy the way the SQL and S3 gateways are built on pkg/client.

It exercises the whole client surface:
- a lease (TTL'd lock, auto-refreshed) identifies the running instance,
- every visit increments a per-name counter in a transaction (safe under
  concurrent instances — OCC conflicts re-run automatically),
- custom greetings are plain KV writes,
- a background watch logs greeting changes as they commit.

    uv run python examples/hello_layer.py --endpoint localhost:18443 --password devpass123

    curl localhost:8090/world
    curl -X PUT localhost:8090/world -d 'Howdy'
    curl localhost:8090/world
"""

from __future__ import annotations

import argparse
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from databoxpy import Databox

PREFIX = "/hello-layer"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="localhost:8443")
    ap.add_argument("--user", default="root")
    ap.add_argument("--password", required=True)
    ap.add_argument("--listen", type=int, default=8090)
    args = ap.parse_args()

    db = Databox(args.endpoint, insecure=True)
    db.login(args.user, args.password)

    lease = db.lease("hello-layer/instance", ttl_ms=10_000,
                     on_lost=lambda err: print(f"lease lost: {err}"))
    print(f"holding lease hello-layer/instance, fencing token {lease.fencing}")

    def watch_greetings() -> None:
        for ev in db.watch(PREFIX + "/greetings/"):
            name = ev.key.rsplit("/", 1)[-1]
            if ev.type == "put":
                print(f"watch: greeting for {name!r} is now {ev.value.decode()!r}")
            else:
                print(f"watch: greeting for {name!r} removed")

    threading.Thread(target=watch_greetings, daemon=True).start()

    class Handler(BaseHTTPRequestHandler):
        def _name(self) -> str:
            return self.path.strip("/") or "world"

        def do_GET(self) -> None:
            name = self._name()
            visits = 0

            def bump(tx) -> None:
                nonlocal visits
                cur = tx.get(f"{PREFIX}/visits/{name}")
                visits = int(cur or b"0") + 1
                tx.set(f"{PREFIX}/visits/{name}", str(visits).encode())

            db.run_tx(bump)
            g = db.get(f"{PREFIX}/greetings/{name}")
            greeting = g.value.decode() if g else "Hello"
            body = f"{greeting}, {name}! (visit #{visits})\n".encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(body)

        def do_PUT(self) -> None:
            name = self._name()
            length = int(self.headers.get("Content-Length", "0"))
            greeting = self.rfile.read(length).decode().strip() or "Hello"
            rev = db.set(f"{PREFIX}/greetings/{name}", greeting.encode())
            self.send_response(200)
            self.end_headers()
            self.wfile.write(f"greeting for {name} set at rev {rev}\n".encode())

        def log_message(self, *_: object) -> None:
            pass  # keep stdout for the watch log

    srv = ThreadingHTTPServer(("127.0.0.1", args.listen), Handler)
    print(f"hello layer listening on :{args.listen}")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        lease.release()
        db.close()


if __name__ == "__main__":
    main()
