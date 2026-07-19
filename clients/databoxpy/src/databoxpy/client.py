"""Databox Python client: a thin wrapper over the HTTPS API that mirrors
the reference Go client (pkg/client) — same retry convention (exponential
backoff with jitter on Conflict/ShardSplitting, five attempts by default),
same client-side OCC transactions with per-shard snapshot pins, same
NDJSON watch protocol. Build layers on it the way the SQL and S3 gateways
are built on pkg/client.
"""

from __future__ import annotations

import base64
import hashlib
import json
import random
import ssl
import threading
import time
import urllib.parse
from dataclasses import dataclass, field
from typing import Any, BinaryIO, Callable, Iterator

import httpx

__all__ = [
    "Databox",
    "Tx",
    "Lease",
    "KVEntry",
    "Event",
    "BlobStat",
    "BlobResult",
    "LockGrant",
    "DataboxError",
    "ConflictError",
    "TxTooOldError",
    "RevisionCompactedError",
    "UnauthorizedError",
    "NotFoundError",
    "ValueTooLargeError",
]

_BACKOFF_START = 0.1
_BACKOFF_CAP = 3.0


class DataboxError(Exception):
    """Base error. `code` is the server's error code (the leading token of
    the message, e.g. "Conflict"), `status` the HTTP status (0 = transport)."""

    def __init__(self, message: str, code: str = "", status: int = 0):
        super().__init__(message)
        self.code = code
        self.status = status


class ConflictError(DataboxError):
    """OCC commit conflict or lock contention (409). For a transaction
    commit this is an answer, not a fault: re-run the transaction body."""


class TxTooOldError(DataboxError):
    """A pinned read version fell behind the MVCC history horizon (410).
    Never retryable at the request level — restart the whole transaction
    with fresh reads (run_tx does this automatically)."""


class RevisionCompactedError(DataboxError):
    """A watch resume revision fell out of the shard's resume buffer (410).
    Re-list from current state and re-subscribe."""


class UnauthorizedError(DataboxError):
    """Authentication or permission failure (401/403)."""


class NotFoundError(DataboxError):
    """No such key/resource (404)."""


class ValueTooLargeError(DataboxError):
    """Value exceeds the cluster's max_value_bytes cap (413). Use blobs."""


def _typed_error(message: str, status: int) -> DataboxError:
    code = message.split(":", 1)[0].strip()
    if code == "Conflict" or code == "LockHeld" or code == "NotHolder":
        return ConflictError(message, code, status)
    if code == "TxTooOld":
        return TxTooOldError(message, code, status)
    if code == "RevisionCompacted":
        return RevisionCompactedError(message, code, status)
    if code == "Unauthorized" or status in (401, 403):
        return UnauthorizedError(message, code, status)
    if code == "NotFound":
        return NotFoundError(message, code, status)
    if code == "ValueTooLarge":
        return ValueTooLargeError(message, code, status)
    return DataboxError(message, code, status)


def _retryable(err: Exception) -> bool:
    # Network errors, 409, and 503 retry per the documented convention.
    if isinstance(err, httpx.TransportError):
        return True
    if isinstance(err, DataboxError):
        return err.status in (409, 503)
    return False


@dataclass
class KVEntry:
    key: str
    value: bytes
    rev: int
    blob: bool = False


@dataclass
class Event:
    """One watch stream event. type is "put" or "delete" (deletes carry no
    value)."""

    rev: int
    type: str
    key: str
    value: bytes = b""
    blob: bool = False


@dataclass
class BlobStat:
    size: int
    content_type: str
    sha256: str = ""


@dataclass
class BlobResult:
    rev: int
    size: int
    sha256: str
    mode: str  # "replica" | "ec"
    composite: bool = False


@dataclass
class LockGrant:
    fencing: int
    holder: str = ""


def _escape_key(key: str) -> str:
    """Render a user key ("/a/b c") as a URL path suffix, keeping slashes
    as separators but escaping everything else."""
    if not key.startswith("/"):
        key = "/" + key
    return "/".join(urllib.parse.quote(p, safe="") for p in key.split("/"))


class _PinnedContext(ssl.SSLContext):
    """TLS context that accepts a certificate only if its SHA-256
    fingerprint matches one of the configured pins — the console's
    trust-on-first-use store model, without interactivity."""

    _pins: set[str] = set()

    def wrap_socket(self, *args, **kwargs):  # type: ignore[override]
        sock = super().wrap_socket(*args, **kwargs)
        der = sock.getpeercert(binary_form=True)
        fp = hashlib.sha256(der or b"").hexdigest().upper()
        fp = ":".join(fp[i : i + 2] for i in range(0, len(fp), 2))
        if fp not in self._pins and fp.replace(":", "") not in self._pins:
            sock.close()
            raise ssl.SSLCertVerificationError(
                f"server certificate {fp} matches no pinned fingerprint"
            )
        return sock


def _make_ssl_context(
    ca_file: str | None, fingerprints: list[str] | None, insecure: bool
) -> ssl.SSLContext | bool:
    if ca_file:
        ctx = ssl.create_default_context(cafile=ca_file)
    elif fingerprints:
        ctx = _PinnedContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        ctx._pins = {f.upper() for f in fingerprints}
    elif insecure:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
    else:
        return True  # system trust store
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    return ctx


class Databox:
    """Client for one databox cluster endpoint.

    endpoint is "host:port" (https is implied and required). Certificate
    trust is one of: ca_file (cluster CA or corporate PKI), fingerprints
    (pinned SHA-256 cert fingerprints), or insecure=True (dev only — no
    verification). Default is the system trust store.
    """

    def __init__(
        self,
        endpoint: str,
        *,
        token: str = "",
        ca_file: str | None = None,
        fingerprints: list[str] | None = None,
        insecure: bool = False,
        retries: int = 5,
        timeout: float = 30.0,
    ):
        if not endpoint:
            raise ValueError("endpoint required")
        self.base = "https://" + endpoint
        self.token = token
        self.retries = retries
        self._http = httpx.Client(
            verify=_make_ssl_context(ca_file, fingerprints, insecure),
            timeout=httpx.Timeout(timeout, read=None),
            http2=False,
        )

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Databox":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # --- transport ------------------------------------------------------

    def _headers(self, json_body: bool = False) -> dict[str, str]:
        h: dict[str, str] = {}
        if self.token:
            h["Authorization"] = "Bearer " + self.token
        if json_body:
            h["Content-Type"] = "application/json"
        return h

    def _once(self, method: str, path: str, body: Any | None) -> Any:
        content = None if body is None else json.dumps(body).encode()
        try:
            resp = self._http.request(
                method,
                self.base + path,
                content=content,
                headers=self._headers(json_body=body is not None),
            )
        except httpx.TransportError:
            raise
        if resp.status_code == 200:
            return resp.json() if resp.content else None
        msg = ""
        try:
            msg = resp.json().get("error", "")
        except Exception:
            pass
        raise _typed_error(msg or f"HTTP {resp.status_code}", resp.status_code)

    def _do(self, method: str, path: str, body: Any | None = None, retries: int | None = None) -> Any:
        attempts = max(1, self.retries if retries is None else retries)
        backoff = _BACKOFF_START
        last: Exception | None = None
        for attempt in range(attempts):
            if attempt > 0:
                time.sleep(backoff + random.uniform(0, backoff / 2))
                backoff = min(backoff * 2, _BACKOFF_CAP)
            try:
                return self._once(method, path, body)
            except Exception as err:  # noqa: BLE001 — classified below
                last = err
                if not _retryable(err):
                    raise
        assert last is not None
        raise last

    def raw(self, method: str, path: str, body: Any | None = None) -> Any:
        """Arbitrary API call — admin endpoints (users, grants, cluster
        status, policies) without a dedicated method for each."""
        return self._do(method, path, body)

    # --- auth -----------------------------------------------------------

    def login(self, username: str, password: str) -> None:
        out = self._do(
            "POST", "/api/v1/auth/login", {"username": username, "password": password}
        )
        self.token = out["token"]

    def logout(self) -> None:
        self._do("POST", "/api/v1/auth/logout")
        self.token = ""

    # --- kv -------------------------------------------------------------

    def get(self, key: str) -> KVEntry | None:
        """Fetch one key (linearizable). None means no such key."""
        try:
            out = self._do("GET", "/api/v1/kv" + _escape_key(key))
        except NotFoundError:
            return None
        return KVEntry(
            key=out["key"],
            value=base64.b64decode(out.get("value") or ""),
            rev=out["rev"],
            blob=out.get("blob", False),
        )

    def set(self, key: str, value: bytes) -> int:
        """Write one key, returning its new revision."""
        out = self._do(
            "PUT",
            "/api/v1/kv" + _escape_key(key),
            {"value": base64.b64encode(value).decode()},
        )
        return out["rev"]

    def delete(self, key: str) -> None:
        self._do("DELETE", "/api/v1/kv" + _escape_key(key))

    def delete_range(self, start: str, end: str) -> None:
        """Remove [start, end)."""
        self._do("POST", "/api/v1/delete-range", {"start": start, "end": end})

    def list(
        self, prefix: str = "/", cursor: str = "", limit: int = 0
    ) -> tuple[list[KVEntry], str]:
        """One page of keys under prefix. Pass the returned cursor to
        continue; empty cursor = end."""
        q = {"prefix": prefix, "cursor": cursor}
        if limit > 0:
            q["limit"] = str(limit)
        out = self._do("GET", "/api/v1/list?" + urllib.parse.urlencode(q))
        entries = [
            KVEntry(
                key=e["key"],
                value=base64.b64decode(e.get("value") or ""),
                rev=e["rev"],
                blob=e.get("blob", False),
            )
            for e in out.get("entries") or []
        ]
        return entries, out.get("next_cursor", "")

    def iter(self, prefix: str = "/", page_size: int = 1000) -> Iterator[KVEntry]:
        """Iterate every key under prefix, paging transparently."""
        cursor = ""
        while True:
            entries, cursor = self.list(prefix, cursor, page_size)
            yield from entries
            if not cursor:
                return

    # --- watch ----------------------------------------------------------

    def watch(self, prefix: str, from_rev: int = 0) -> Iterator[Event]:
        """Stream change events under prefix until the caller stops
        iterating or the stream breaks. from_rev resumes a single-shard
        watch; a compacted resume raises RevisionCompactedError (re-list
        and re-subscribe from current state).
        """
        q: dict[str, str] = {"prefix": prefix}
        if from_rev > 0:
            q["from_revision"] = str(from_rev)
        with self._http.stream(
            "GET",
            self.base + "/api/v1/watch?" + urllib.parse.urlencode(q),
            headers=self._headers(),
        ) as resp:
            if resp.status_code != 200:
                raw = resp.read()
                msg = ""
                try:
                    msg = json.loads(raw).get("error", "")
                except Exception:
                    pass
                raise _typed_error(msg or f"HTTP {resp.status_code}", resp.status_code)
            for line in resp.iter_lines():
                if not line.strip():
                    continue
                obj = json.loads(line)
                if obj.get("error"):
                    # A stream line is either an event or a terminal server
                    # error — never delivered to the caller as an event.
                    raise _typed_error(obj["error"], 200)
                yield Event(
                    rev=obj["rev"],
                    type=obj["type"],
                    key=obj["key"],
                    value=base64.b64decode(obj.get("value") or ""),
                    blob=obj.get("blob", False),
                )

    # --- transactions ---------------------------------------------------

    def tx(self) -> "Tx":
        """Start a client-side transaction (snapshot reads per shard,
        OCC-validated at commit)."""
        return Tx(self)

    def run_tx(self, fn: Callable[["Tx"], None]) -> None:
        """Run fn inside a transaction and commit, restarting the whole
        transaction (fresh reads) with backoff on commit Conflict or
        TxTooOld. Any other error returns immediately."""
        attempts = max(1, self.retries)
        backoff = _BACKOFF_START
        last: Exception | None = None
        for attempt in range(attempts):
            if attempt > 0:
                time.sleep(backoff + random.uniform(0, backoff / 2))
                backoff = min(backoff * 2, _BACKOFF_CAP)
            t = self.tx()
            try:
                fn(t)
                t.commit()
                return
            except (ConflictError, TxTooOldError) as err:
                last = err
                continue
        assert last is not None
        raise last

    # --- locks / leases -------------------------------------------------

    def lock_acquire(
        self,
        resource: str,
        mode: str = "exclusive",
        ttl_ms: int = 30_000,
        handle: str = "",
    ) -> LockGrant:
        """Take or refresh a lock; returns the fencing token — a monotonic
        integer to fence out stale holders. Contention raises ConflictError
        (LockHeld) after the retry budget."""
        out = self._do(
            "POST",
            "/api/v1/locks/acquire",
            {"resource": resource, "mode": mode, "ttl_ms": ttl_ms, "handle": handle},
        )
        return LockGrant(fencing=out["fencing"], holder=out.get("holder", ""))

    def lock_release(self, resource: str, handle: str = "") -> None:
        self._do(
            "POST", "/api/v1/locks/release", {"resource": resource, "handle": handle}
        )

    def lock_status(self, resource: str) -> dict[str, Any]:
        """{"locked": bool, "state": ...} for a resource. Lock resources
        are plain names (no leading slash), unlike keyspace keys."""
        escaped = "/".join(
            urllib.parse.quote(p, safe="") for p in resource.split("/")
        )
        return self._do("GET", "/api/v1/locks/" + escaped)

    def lease(
        self,
        resource: str,
        ttl_ms: int = 30_000,
        mode: str = "exclusive",
        handle: str = "",
        on_lost: Callable[[Exception], None] | None = None,
    ) -> "Lease":
        """Acquire a lock and keep it alive from a background thread,
        refreshing at ttl/3. Use as a context manager or call release().
        Blocks (with the retry budget) until acquired."""
        grant = self.lock_acquire(resource, mode, ttl_ms, handle)
        return Lease(self, resource, mode, ttl_ms, handle, grant, on_lost)

    # --- blobs ----------------------------------------------------------

    def _blob_request(
        self,
        method: str,
        key: str,
        content: bytes | BinaryIO | None = None,
        content_type: str = "",
        query: dict[str, str] | None = None,
    ) -> httpx.Response:
        url = self.base + "/api/v1/blobs" + _escape_key(key)
        if query:
            url += "?" + urllib.parse.urlencode(query)
        headers = self._headers()
        if content_type:
            headers["Content-Type"] = content_type
        return self._http.request(method, url, content=content, headers=headers)

    @staticmethod
    def _blob_result(resp: httpx.Response) -> BlobResult:
        out = resp.json()
        return BlobResult(
            rev=out["rev"],
            size=out["size"],
            sha256=out.get("sha256", ""),
            mode=out.get("mode", ""),
            composite=out.get("composite", False),
        )

    @staticmethod
    def _blob_error(resp: httpx.Response) -> DataboxError:
        msg = ""
        try:
            msg = resp.json().get("error", "")
        except Exception:
            pass
        return _typed_error(msg or f"HTTP {resp.status_code}", resp.status_code)

    def put_blob(
        self, key: str, data: bytes | BinaryIO, content_type: str = ""
    ) -> BlobResult:
        """Store a blob (raw bytes stream; no size cap like KV values)."""
        resp = self._blob_request("PUT", key, data, content_type)
        if resp.status_code != 200:
            raise self._blob_error(resp)
        return self._blob_result(resp)

    def append_blob(self, key: str, data: bytes | BinaryIO) -> BlobResult:
        """Extend a blob. ConflictError means a concurrent append won —
        retry (the failed attempt's data never became visible)."""
        resp = self._blob_request("PATCH", key, data)
        if resp.status_code != 200:
            raise self._blob_error(resp)
        return self._blob_result(resp)

    def get_blob(self, key: str) -> bytes:
        resp = self._blob_request("GET", key)
        if resp.status_code != 200:
            raise self._blob_error(resp)
        return resp.content

    def get_blob_range(self, key: str, offset: int, length: int = -1) -> bytes:
        """length < 0 = to the end. The server never touches chunks outside
        the window — the primitive HTTP Range serving builds on."""
        q = {"offset": str(offset)}
        if length >= 0:
            q["length"] = str(length)
        resp = self._blob_request("GET", key, query=q)
        if resp.status_code != 200:
            raise self._blob_error(resp)
        return resp.content

    def stream_blob(self, key: str, chunk_size: int = 1 << 20) -> Iterator[bytes]:
        """Stream a blob's bytes without buffering it whole."""
        url = self.base + "/api/v1/blobs" + _escape_key(key)
        with self._http.stream("GET", url, headers=self._headers()) as resp:
            if resp.status_code != 200:
                resp.read()
                raise self._blob_error(resp)
            yield from resp.iter_bytes(chunk_size)

    def stat_blob(self, key: str) -> BlobStat | None:
        """Size/content-type/hash without reading data. None = no blob."""
        resp = self._blob_request("HEAD", key)
        if resp.status_code == 404:
            return None
        if resp.status_code != 200:
            raise self._blob_error(resp)
        return BlobStat(
            size=int(resp.headers.get("Content-Length", "0")),
            content_type=resp.headers.get("Content-Type", ""),
            sha256=resp.headers.get("X-Databox-SHA256", ""),
        )

    def delete_blob(self, key: str) -> None:
        self._do("DELETE", "/api/v1/blobs" + _escape_key(key))

    def splice_blobs(
        self, destination: str, sources: list[str], content_type: str = ""
    ) -> BlobResult:
        """Concatenate the blobs at sources, in order, into destination —
        server-side; no blob data streams through the client. Multi-source
        destinations carry a composite hash and refuse append_blob."""
        out = self._do(
            "POST",
            "/api/v1/blobs-splice",
            {"destination": destination, "sources": sources, "content_type": content_type},
        )
        return BlobResult(
            rev=out["rev"],
            size=out["size"],
            sha256=out.get("sha256", ""),
            mode=out.get("mode", ""),
            composite=out.get("composite", False),
        )


class Tx:
    """Client-side transaction: accumulates a read set and write set.

        tx = db.tx()
        v = tx.get("/a")              # records the revision read
        tx.set("/b", (v or b"") + b"!")
        tx.commit()                   # ConflictError → re-run the body

    Reads are SNAPSHOT reads per shard: the first read against a shard pins
    that shard's revision, and every later read against the same shard
    executes at the pinned revision. A pin that ages past the MVCC horizon
    raises TxTooOldError; restart the transaction (run_tx automates this).
    """

    def __init__(self, client: Databox):
        self._c = client
        self._reads: dict[str, int] = {}
        self._writes: list[dict[str, Any]] = []
        # cache provides read-your-writes inside the transaction.
        self._cache: dict[str, bytes | None] = {}
        # pins: shard group ID → the shard revision this tx reads at.
        self._pins: dict[int, int] = {}

    def read_versions(self) -> dict[int, int]:
        """Per-shard read versions pinned so far (diagnostics)."""
        return dict(self._pins)

    def _pins_param(self) -> str:
        return ",".join(f"{gid}:{rev}" for gid, rev in self._pins.items())

    def _pin(self, gid: int, shard_rev: int) -> None:
        if gid > 0 and gid not in self._pins:
            self._pins[gid] = shard_rev

    def get(self, key: str) -> bytes | None:
        """Read through the transaction: staged writes are visible, base
        reads execute at the shard's pinned read version and record the
        revision seen. None = no such key."""
        if not key.startswith("/"):
            key = "/" + key
        if key in self._cache:
            return self._cache[key]
        q = {"tx": "1"}
        if self._pins:
            q["pins"] = self._pins_param()
        out = self._c._do(
            "GET", "/api/v1/kv" + _escape_key(key) + "?" + urllib.parse.urlencode(q)
        )
        self._pin(out.get("gid", 0), out.get("shard_rev", 0))
        if out.get("found"):
            self._reads[key] = out["rev"]
            return base64.b64decode(out.get("value") or "")
        self._reads[key] = 0  # "did not exist" is also a read to validate
        return None

    def list(
        self, prefix: str, cursor: str = "", limit: int = 0
    ) -> tuple[list[KVEntry], str]:
        """Scan a prefix at the transaction's snapshot. Every returned key
        joins the read set; keys absent from the result are NOT validated
        (phantom inserts do not conflict). Staged writes are not merged in.
        """
        q = {"prefix": prefix, "cursor": cursor, "pins": self._pins_param()}
        if limit > 0:
            q["limit"] = str(limit)
        out = self._c._do("GET", "/api/v1/list?" + urllib.parse.urlencode(q))
        for gid, rev in (out.get("shard_revs") or {}).items():
            self._pin(int(gid), rev)
        entries = [
            KVEntry(
                key=e["key"],
                value=base64.b64decode(e.get("value") or ""),
                rev=e["rev"],
                blob=e.get("blob", False),
            )
            for e in out.get("entries") or []
        ]
        for e in entries:
            if e.key not in self._cache:
                self._reads[e.key] = e.rev
        return entries, out.get("next_cursor", "")

    def set(self, key: str, value: bytes) -> None:
        """Stage a write."""
        if not key.startswith("/"):
            key = "/" + key
        self._cache[key] = bytes(value)
        self._writes.append(
            {"key": key, "value": base64.b64encode(value).decode()}
        )

    def delete(self, key: str) -> None:
        """Stage a deletion."""
        if not key.startswith("/"):
            key = "/" + key
        self._cache[key] = None
        self._writes.append({"key": key, "delete": True})

    def commit(self) -> None:
        """Submit the transaction. ConflictError means another writer won;
        re-run the transaction body and commit again (or use run_tx)."""
        if not self._writes:
            return  # read-only transactions validate trivially
        # Commit must NOT ride the generic retry loop: a Conflict result is
        # a real answer, not a transient fault.
        self._c._do(
            "POST",
            "/api/v1/tx/commit",
            {"reads": self._reads, "writes": self._writes},
            retries=1,
        )


class Lease:
    """A TTL'd lock kept alive by a background refresh thread (refresh
    period = ttl/3). The fencing token fences out stale holders: pass it to
    whatever the lease guards and reject smaller tokens there.
    """

    def __init__(
        self,
        client: Databox,
        resource: str,
        mode: str,
        ttl_ms: int,
        handle: str,
        grant: LockGrant,
        on_lost: Callable[[Exception], None] | None,
    ):
        self._c = client
        self.resource = resource
        self.mode = mode
        self.ttl_ms = ttl_ms
        self.handle = handle
        self.fencing = grant.fencing
        self.holder = grant.holder
        self._on_lost = on_lost
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._refresh_loop, daemon=True)
        self._thread.start()

    @property
    def alive(self) -> bool:
        return not self._stop.is_set()

    def _refresh_loop(self) -> None:
        period = max(self.ttl_ms / 3000.0, 0.05)
        while not self._stop.wait(period):
            try:
                grant = self._c.lock_acquire(
                    self.resource, self.mode, self.ttl_ms, self.handle
                )
                self.fencing = grant.fencing
            except Exception as err:  # noqa: BLE001 — reported via on_lost
                self._stop.set()
                if self._on_lost:
                    self._on_lost(err)
                return

    def release(self) -> None:
        """Stop refreshing and release the lock."""
        self._stop.set()
        self._thread.join(timeout=5)
        try:
            self._c.lock_release(self.resource, self.handle)
        except ConflictError:
            pass  # already expired or force-unlocked

    def __enter__(self) -> "Lease":
        return self

    def __exit__(self, *exc: object) -> None:
        self.release()
