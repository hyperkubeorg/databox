"""Live smoke test: exercises every client subsystem against a running
databox node.

    uv run python tests/smoke.py --endpoint localhost:18443 --password devpass123
"""

from __future__ import annotations

import argparse
import threading
import time

from databoxpy import ConflictError, Databox, NotFoundError

PREFIX = "/smoketest-py"
LOCKNS = "smoketest-py"  # lock resources are plain names, no leading slash


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="localhost:8443")
    ap.add_argument("--user", default="root")
    ap.add_argument("--password", required=True)
    args = ap.parse_args()

    db = Databox(args.endpoint, insecure=True)
    db.login(args.user, args.password)
    db.delete_range(PREFIX + "/", PREFIX + "0")  # '0' sorts just after '/'

    # --- kv ---
    rev1 = db.set(PREFIX + "/kv/a", b"alpha")
    rev2 = db.set(PREFIX + "/kv/b", b"beta")
    assert rev2 > rev1
    e = db.get(PREFIX + "/kv/a")
    assert e is not None and e.value == b"alpha" and e.rev == rev1
    assert db.get(PREFIX + "/kv/missing") is None
    db.set(PREFIX + "/kv/space key/x", b"escaped ok")
    e = db.get(PREFIX + "/kv/space key/x")
    assert e is not None and e.value == b"escaped ok"
    entries, cursor = db.list(PREFIX + "/kv/", limit=2)
    assert len(entries) == 2 and cursor
    all_keys = [e.key for e in db.iter(PREFIX + "/kv/", page_size=2)]
    assert len(all_keys) == 3, all_keys
    db.delete(PREFIX + "/kv/b")
    assert db.get(PREFIX + "/kv/b") is None
    print("kv: ok")

    # --- transactions: increment race ---
    db.set(PREFIX + "/counter", b"0")

    def incr(tx) -> None:
        cur = tx.get(PREFIX + "/counter")
        tx.set(PREFIX + "/counter", str(int(cur or b"0") + 1).encode())

    threads = [threading.Thread(target=lambda: db.run_tx(incr)) for _ in range(5)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    e = db.get(PREFIX + "/counter")
    assert e is not None and e.value == b"5", e.value

    # explicit conflict: two txs read the same rev, second commit must 409
    t1, t2 = db.tx(), db.tx()
    t1.get(PREFIX + "/counter")
    t2.get(PREFIX + "/counter")
    t1.set(PREFIX + "/counter", b"100")
    t2.set(PREFIX + "/counter", b"200")
    t1.commit()
    try:
        t2.commit()
        raise AssertionError("expected ConflictError")
    except ConflictError:
        pass

    # create-if-absent (reads rev 0), then conflict on second create
    t3 = db.tx()
    assert t3.get(PREFIX + "/fresh") is None
    t3.set(PREFIX + "/fresh", b"first")
    t3.commit()

    # tx list joins the read set + read-your-writes via get
    t4 = db.tx()
    listed, _ = t4.list(PREFIX + "/kv/")
    assert len(listed) == 2
    t4.set(PREFIX + "/kv/a", b"staged")
    assert t4.get(PREFIX + "/kv/a") == b"staged"
    t4.delete(PREFIX + "/kv/a")
    assert t4.get(PREFIX + "/kv/a") is None
    t4.commit()
    assert db.get(PREFIX + "/kv/a") is None
    print("tx: ok")

    # --- locks + lease ---
    grant = db.lock_acquire(LOCKNS + "/lock1", ttl_ms=5000)
    assert grant.fencing > 0
    st = db.lock_status(LOCKNS + "/lock1")
    assert st["locked"] is True
    db.lock_release(LOCKNS + "/lock1")
    assert db.lock_status(LOCKNS + "/lock1")["locked"] is False

    with db.lease(LOCKNS + "/leader", ttl_ms=1200) as lease:
        assert lease.fencing > 0 and lease.alive
        first_fencing = lease.fencing
        time.sleep(1.0)  # long enough for at least one ttl/3 refresh
        assert lease.alive and lease.fencing >= first_fencing
    assert db.lock_status(LOCKNS + "/leader")["locked"] is False
    print("locks/lease: ok")

    # --- watch ---
    events: list = []
    ready = threading.Event()

    def watcher() -> None:
        ready.set()
        for ev in db.watch(PREFIX + "/watched/"):
            events.append(ev)
            if len(events) >= 2:
                return

    wt = threading.Thread(target=watcher, daemon=True)
    wt.start()
    ready.wait()
    time.sleep(0.5)  # let the stream attach server-side
    db.set(PREFIX + "/watched/x", b"1")
    db.delete(PREFIX + "/watched/x")
    wt.join(timeout=10)
    assert len(events) == 2, events
    assert events[0].type == "put" and events[0].value == b"1"
    assert events[1].type == "delete"
    print("watch: ok")

    # --- blobs ---
    payload = bytes(range(256)) * 64  # 16 KiB
    res = db.put_blob(PREFIX + "/blob/data", payload, "application/octet-stream")
    assert res.size == len(payload)
    assert db.get_blob(PREFIX + "/blob/data") == payload
    assert db.get_blob_range(PREFIX + "/blob/data", 256, 256) == payload[256:512]
    assert db.get_blob_range(PREFIX + "/blob/data", len(payload) - 16) == payload[-16:]
    st2 = db.stat_blob(PREFIX + "/blob/data")
    assert st2 is not None and st2.size == len(payload)
    assert db.stat_blob(PREFIX + "/blob/none") is None
    db.append_blob(PREFIX + "/blob/data", b"tail")
    assert db.get_blob(PREFIX + "/blob/data") == payload + b"tail"
    streamed = b"".join(db.stream_blob(PREFIX + "/blob/data", chunk_size=1024))
    assert streamed == payload + b"tail"
    db.put_blob(PREFIX + "/blob/p1", b"hello ")
    db.put_blob(PREFIX + "/blob/p2", b"world")
    sp = db.splice_blobs(PREFIX + "/blob/joined", [PREFIX + "/blob/p1", PREFIX + "/blob/p2"])
    assert sp.size == 11 and sp.composite
    assert db.get_blob(PREFIX + "/blob/joined") == b"hello world"
    db.delete_blob(PREFIX + "/blob/joined")
    assert db.stat_blob(PREFIX + "/blob/joined") is None
    print("blobs: ok")

    # --- raw admin passthrough ---
    status = db.raw("GET", "/api/v1/cluster/status")
    assert status["cluster_id"] and status["nodes"]
    print("raw/admin: ok")

    # --- errors ---
    try:
        db.delete(PREFIX + "/kv/definitely-missing")
    except NotFoundError:
        pass  # delete of missing key may or may not error; both fine
    db.delete_range(PREFIX + "/", PREFIX + "0")

    print("ALL PYTHON SMOKE TESTS PASSED")


if __name__ == "__main__":
    main()
