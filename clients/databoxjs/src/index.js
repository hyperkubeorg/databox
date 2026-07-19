/**
 * databoxjs — JavaScript client for the databox database (Bun runtime).
 *
 * A thin wrapper over the HTTPS API mirroring the reference Go client
 * (pkg/client): same retry convention (exponential backoff with jitter on
 * Conflict/ShardSplitting, five attempts by default), same client-side OCC
 * transactions with per-shard snapshot pins, same NDJSON watch protocol.
 * Build layers on it the way the SQL and S3 gateways are built on
 * pkg/client.
 */

const BACKOFF_START_MS = 100;
const BACKOFF_CAP_MS = 3000;

export class DataboxError extends Error {
  /**
   * @param {string} message
   * @param {string} [code] server error code — leading token of the message
   * @param {number} [status] HTTP status (0 = transport failure)
   */
  constructor(message, code = "", status = 0) {
    super(message);
    this.name = new.target.name;
    this.code = code;
    this.status = status;
  }
}

/** OCC commit conflict or lock contention (409). For a transaction commit
 * this is an answer, not a fault: re-run the transaction body. */
export class ConflictError extends DataboxError {}

/** A pinned read version fell behind the MVCC history horizon (410). Never
 * retryable at the request level — restart the whole transaction with
 * fresh reads (runTx does this automatically). */
export class TxTooOldError extends DataboxError {}

/** A watch resume revision fell out of the shard's resume buffer (410).
 * Re-list from current state and re-subscribe. */
export class RevisionCompactedError extends DataboxError {}

/** Authentication or permission failure (401/403). */
export class UnauthorizedError extends DataboxError {}

/** No such key/resource (404). */
export class NotFoundError extends DataboxError {}

/** Value exceeds the cluster's max_value_bytes cap (413). Use blobs. */
export class ValueTooLargeError extends DataboxError {}

/**
 * @param {string} message
 * @param {number} status
 * @returns {DataboxError}
 */
function typedError(message, status) {
  const code = message.split(":", 1)[0].trim();
  switch (code) {
    case "Conflict":
    case "LockHeld":
    case "NotHolder":
      return new ConflictError(message, code, status);
    case "TxTooOld":
      return new TxTooOldError(message, code, status);
    case "RevisionCompacted":
      return new RevisionCompactedError(message, code, status);
    case "NotFound":
      return new NotFoundError(message, code, status);
    case "ValueTooLarge":
      return new ValueTooLargeError(message, code, status);
  }
  if (code === "Unauthorized" || status === 401 || status === 403) {
    return new UnauthorizedError(message, code, status);
  }
  return new DataboxError(message, code, status);
}

function retryable(err) {
  // Network errors, 409, and 503 retry per the documented convention.
  if (err instanceof DataboxError) return err.status === 409 || err.status === 503;
  return !(err instanceof Error && err.name === "AbortError");
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * @typedef {Object} KVEntry
 * @property {string} key
 * @property {Uint8Array} value
 * @property {number} rev
 * @property {boolean} blob
 */

/**
 * @typedef {Object} WatchEvent one watch stream event; deletes carry no value
 * @property {number} rev
 * @property {"put"|"delete"} type
 * @property {string} key
 * @property {Uint8Array} value
 * @property {boolean} blob
 */

/** @param {Uint8Array|string} v */
function toBytes(v) {
  return typeof v === "string" ? new TextEncoder().encode(v) : v;
}

/** @param {Uint8Array} v */
function b64encode(v) {
  return Buffer.from(v).toString("base64");
}

/** @param {string|undefined} s */
function b64decode(s) {
  return s ? new Uint8Array(Buffer.from(s, "base64")) : new Uint8Array(0);
}

/** Render a user key ("/a/b c") as a URL path suffix: slashes stay
 * separators, everything else is escaped.
 * @param {string} key */
function escapeKey(key) {
  if (!key.startsWith("/")) key = "/" + key;
  return key.split("/").map(encodeURIComponent).join("/");
}

/** Lock resources are plain names (no leading slash), unlike keyspace keys.
 * @param {string} resource */
function escapeResource(resource) {
  return resource.split("/").map(encodeURIComponent).join("/");
}

function parseEntry(e) {
  return { key: e.key, value: b64decode(e.value), rev: e.rev, blob: Boolean(e.blob) };
}

/** Client for one databox cluster endpoint. */
export class Databox {
  /**
   * @param {Object} opts
   * @param {string} opts.endpoint "host:port" — https implied and required
   * @param {string} [opts.token] pre-set a session token (otherwise login)
   * @param {string} [opts.ca] PEM CA certificate(s) verifying the cluster
   * @param {boolean} [opts.insecure] skip certificate verification — dev only
   * @param {number} [opts.retries] attempt budget for retryable errors (default 5)
   */
  constructor(opts) {
    if (!opts?.endpoint) throw new Error("endpoint required");
    this.base = "https://" + opts.endpoint;
    this.token = opts.token ?? "";
    this.retries = opts.retries ?? 5;
    /** @private */
    this._tls = opts.ca ? { ca: opts.ca } : opts.insecure ? { rejectUnauthorized: false } : undefined;
  }

  /** @private */
  _headers(json = false) {
    const h = {};
    if (this.token) h.Authorization = "Bearer " + this.token;
    if (json) h["Content-Type"] = "application/json";
    return h;
  }

  /** One fetch with Bun's TLS options applied. @private */
  _fetch(path, init) {
    return fetch(this.base + path, { ...init, tls: this._tls });
  }

  /** @private */
  async _once(method, path, body) {
    const resp = await this._fetch(path, {
      method,
      headers: this._headers(body !== undefined),
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (resp.status === 200) {
      const text = await resp.text();
      return text ? JSON.parse(text) : undefined;
    }
    let msg = "";
    try {
      msg = (await resp.json()).error ?? "";
    } catch {}
    throw typedError(msg || `HTTP ${resp.status}`, resp.status);
  }

  /** @private */
  async _do(method, path, body, retries) {
    const attempts = Math.max(1, retries ?? this.retries);
    let backoff = BACKOFF_START_MS;
    let last;
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (attempt > 0) {
        await sleep(backoff + Math.random() * (backoff / 2));
        backoff = Math.min(backoff * 2, BACKOFF_CAP_MS);
      }
      try {
        return await this._once(method, path, body);
      } catch (err) {
        last = err;
        if (!retryable(err)) throw err;
      }
    }
    throw last;
  }

  /** Arbitrary API call — admin endpoints (users, grants, cluster status,
   * policies) without a dedicated method for each.
   * @param {string} method @param {string} path @param {unknown} [body] */
  raw(method, path, body) {
    return this._do(method, path, body);
  }

  // --- auth -----------------------------------------------------------

  /** @param {string} username @param {string} password */
  async login(username, password) {
    const out = await this._do("POST", "/api/v1/auth/login", { username, password });
    this.token = out.token;
  }

  async logout() {
    await this._do("POST", "/api/v1/auth/logout");
    this.token = "";
  }

  // --- kv -------------------------------------------------------------

  /** Fetch one key (linearizable). null means no such key.
   * @param {string} key @returns {Promise<KVEntry|null>} */
  async get(key) {
    try {
      return parseEntry(await this._do("GET", "/api/v1/kv" + escapeKey(key)));
    } catch (err) {
      if (err instanceof NotFoundError) return null;
      throw err;
    }
  }

  /** Write one key, returning its new revision.
   * @param {string} key @param {Uint8Array|string} value @returns {Promise<number>} */
  async set(key, value) {
    const out = await this._do("PUT", "/api/v1/kv" + escapeKey(key), {
      value: b64encode(toBytes(value)),
    });
    return out.rev;
  }

  /** @param {string} key */
  async delete(key) {
    await this._do("DELETE", "/api/v1/kv" + escapeKey(key));
  }

  /** Remove [start, end).
   * @param {string} start @param {string} end */
  async deleteRange(start, end) {
    await this._do("POST", "/api/v1/delete-range", { start, end });
  }

  /** One page of keys under prefix. Empty nextCursor = end.
   * @param {string} [prefix] @param {{cursor?: string, limit?: number}} [opts]
   * @returns {Promise<{entries: KVEntry[], nextCursor: string}>} */
  async list(prefix = "/", opts = {}) {
    const q = new URLSearchParams({ prefix, cursor: opts.cursor ?? "" });
    if (opts.limit) q.set("limit", String(opts.limit));
    const out = await this._do("GET", "/api/v1/list?" + q);
    return {
      entries: (out.entries ?? []).map(parseEntry),
      nextCursor: out.next_cursor ?? "",
    };
  }

  /** Iterate every key under prefix, paging transparently.
   * @param {string} [prefix] @param {number} [pageSize]
   * @returns {AsyncGenerator<KVEntry>} */
  async *iter(prefix = "/", pageSize = 1000) {
    let cursor = "";
    for (;;) {
      const { entries, nextCursor } = await this.list(prefix, { cursor, limit: pageSize });
      yield* entries;
      if (!nextCursor) return;
      cursor = nextCursor;
    }
  }

  // --- watch ----------------------------------------------------------

  /**
   * Stream change events under prefix until the caller stops iterating,
   * the signal aborts, or the stream breaks. fromRev resumes a
   * single-shard watch; a compacted resume throws RevisionCompactedError
   * (re-list from current state and re-subscribe).
   * @param {string} prefix
   * @param {{fromRev?: number, signal?: AbortSignal}} [opts]
   * @returns {AsyncGenerator<WatchEvent>}
   */
  async *watch(prefix, opts = {}) {
    const q = new URLSearchParams({ prefix });
    if (opts.fromRev) q.set("from_revision", String(opts.fromRev));
    const resp = await this._fetch("/api/v1/watch?" + q, {
      method: "GET",
      headers: this._headers(),
      signal: opts.signal,
    });
    if (resp.status !== 200) {
      let msg = "";
      try {
        msg = (await resp.json()).error ?? "";
      } catch {}
      throw typedError(msg || `HTTP ${resp.status}`, resp.status);
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        buf += decoder.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          const obj = JSON.parse(line);
          if (obj.error) {
            // A stream line is either an event or a terminal server
            // error — never delivered to the caller as an event.
            throw typedError(obj.error, 200);
          }
          yield {
            rev: obj.rev,
            type: obj.type,
            key: obj.key,
            value: b64decode(obj.value),
            blob: Boolean(obj.blob),
          };
        }
      }
    } finally {
      await reader.cancel().catch(() => {});
    }
  }

  // --- transactions ---------------------------------------------------

  /** Start a client-side transaction (snapshot reads per shard,
   * OCC-validated at commit). @returns {Tx} */
  tx() {
    return new Tx(this);
  }

  /**
   * Run fn inside a transaction and commit, restarting the whole
   * transaction (fresh reads) with backoff on commit Conflict or
   * TxTooOld. Any other error rejects immediately.
   * @param {(tx: Tx) => Promise<void>|void} fn
   */
  async runTx(fn) {
    const attempts = Math.max(1, this.retries);
    let backoff = BACKOFF_START_MS;
    let last;
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (attempt > 0) {
        await sleep(backoff + Math.random() * (backoff / 2));
        backoff = Math.min(backoff * 2, BACKOFF_CAP_MS);
      }
      const t = this.tx();
      try {
        await fn(t);
        await t.commit();
        return;
      } catch (err) {
        if (err instanceof ConflictError || err instanceof TxTooOldError) {
          last = err;
          continue;
        }
        throw err;
      }
    }
    throw last;
  }

  // --- locks / leases -------------------------------------------------

  /** Take or refresh a lock; returns the fencing token — a monotonic
   * integer to fence out stale holders. Contention throws ConflictError
   * (LockHeld) after the retry budget. Lock resources are plain names
   * (no leading slash), unlike keyspace keys.
   * @param {string} resource
   * @param {{mode?: "exclusive"|"shared", ttlMs?: number, handle?: string}} [opts]
   * @returns {Promise<{fencing: number, holder: string}>} */
  async lockAcquire(resource, opts = {}) {
    const out = await this._do("POST", "/api/v1/locks/acquire", {
      resource,
      mode: opts.mode ?? "exclusive",
      ttl_ms: opts.ttlMs ?? 30_000,
      handle: opts.handle ?? "",
    });
    return { fencing: out.fencing, holder: out.holder ?? "" };
  }

  /** @param {string} resource @param {string} [handle] */
  async lockRelease(resource, handle = "") {
    await this._do("POST", "/api/v1/locks/release", { resource, handle });
  }

  /** {locked: boolean, state?: ...} for a resource.
   * @param {string} resource */
  lockStatus(resource) {
    return this._do("GET", "/api/v1/locks/" + escapeResource(resource));
  }

  /**
   * Acquire a lock and keep it alive with a timer, refreshing at ttl/3.
   * Resolves once acquired (with the retry budget); call release() when
   * done. The fencing token fences out stale holders: pass it to whatever
   * the lease guards and reject smaller tokens there.
   * @param {string} resource
   * @param {{ttlMs?: number, mode?: "exclusive"|"shared", handle?: string,
   *          onLost?: (err: unknown) => void}} [opts]
   * @returns {Promise<Lease>}
   */
  async lease(resource, opts = {}) {
    const ttlMs = opts.ttlMs ?? 30_000;
    const grant = await this.lockAcquire(resource, { mode: opts.mode, ttlMs, handle: opts.handle });
    return new Lease(this, resource, opts.mode ?? "exclusive", ttlMs, opts.handle ?? "", grant, opts.onLost);
  }

  // --- blobs ----------------------------------------------------------

  /** @private */
  async _blobRequest(method, key, body, contentType = "", query) {
    let path = "/api/v1/blobs" + escapeKey(key);
    if (query) path += "?" + new URLSearchParams(query);
    const headers = this._headers();
    if (contentType) headers["Content-Type"] = contentType;
    return this._fetch(path, { method, headers, body });
  }

  /** @private */
  static async _blobError(resp) {
    let msg = "";
    try {
      msg = (await resp.json()).error ?? "";
    } catch {}
    return typedError(msg || `HTTP ${resp.status}`, resp.status);
  }

  /** @private */
  static async _blobResult(resp) {
    const out = await resp.json();
    return {
      rev: out.rev,
      size: out.size,
      sha256: out.sha256 ?? "",
      mode: out.mode ?? "",
      composite: Boolean(out.composite),
    };
  }

  /** Store a blob (raw byte stream; no size cap like KV values).
   * @param {string} key @param {Uint8Array|string|Blob|ReadableStream} data
   * @param {string} [contentType] */
  async putBlob(key, data, contentType = "") {
    const body = typeof data === "string" ? toBytes(data) : data;
    const resp = await this._blobRequest("PUT", key, body, contentType);
    if (resp.status !== 200) throw await Databox._blobError(resp);
    return Databox._blobResult(resp);
  }

  /** Extend a blob. ConflictError means a concurrent append won — retry
   * (the failed attempt's data never became visible).
   * @param {string} key @param {Uint8Array|string|Blob} data */
  async appendBlob(key, data) {
    const body = typeof data === "string" ? toBytes(data) : data;
    const resp = await this._blobRequest("PATCH", key, body);
    if (resp.status !== 200) throw await Databox._blobError(resp);
    return Databox._blobResult(resp);
  }

  /** @param {string} key @returns {Promise<Uint8Array>} */
  async getBlob(key) {
    const resp = await this._blobRequest("GET", key);
    if (resp.status !== 200) throw await Databox._blobError(resp);
    return new Uint8Array(await resp.arrayBuffer());
  }

  /** length < 0 = to the end. The server never touches chunks outside the
   * window — the primitive HTTP Range serving builds on.
   * @param {string} key @param {number} offset @param {number} [length] */
  async getBlobRange(key, offset, length = -1) {
    const q = { offset: String(offset) };
    if (length >= 0) q.length = String(length);
    const resp = await this._blobRequest("GET", key, undefined, "", q);
    if (resp.status !== 200) throw await Databox._blobError(resp);
    return new Uint8Array(await resp.arrayBuffer());
  }

  /** Stream a blob's bytes without buffering it whole.
   * @param {string} key @returns {Promise<ReadableStream<Uint8Array>>} */
  async streamBlob(key) {
    const resp = await this._blobRequest("GET", key);
    if (resp.status !== 200) throw await Databox._blobError(resp);
    return resp.body;
  }

  /** Size/content-type/hash without reading data. null = no blob.
   * @param {string} key
   * @returns {Promise<{size: number, contentType: string, sha256: string}|null>} */
  async statBlob(key) {
    const resp = await this._blobRequest("HEAD", key);
    await resp.arrayBuffer(); // drain
    if (resp.status === 404) return null;
    if (resp.status !== 200) throw typedError(`HTTP ${resp.status}`, resp.status);
    return {
      size: Number(resp.headers.get("Content-Length") ?? 0),
      contentType: resp.headers.get("Content-Type") ?? "",
      sha256: resp.headers.get("X-Databox-SHA256") ?? "",
    };
  }

  /** @param {string} key */
  async deleteBlob(key) {
    await this._do("DELETE", "/api/v1/blobs" + escapeKey(key));
  }

  /** Concatenate the blobs at sources, in order, into destination —
   * server-side; no blob data streams through the client. Multi-source
   * destinations carry a composite hash and refuse appendBlob.
   * @param {string} destination @param {string[]} sources @param {string} [contentType] */
  async spliceBlobs(destination, sources, contentType = "") {
    const out = await this._do("POST", "/api/v1/blobs-splice", {
      destination,
      sources,
      content_type: contentType,
    });
    return {
      rev: out.rev,
      size: out.size,
      sha256: out.sha256 ?? "",
      mode: out.mode ?? "",
      composite: Boolean(out.composite),
    };
  }
}

/**
 * Client-side transaction: accumulates a read set and write set.
 *
 *     const tx = db.tx();
 *     const v = await tx.get("/a");        // records the revision read
 *     tx.set("/b", "updated");
 *     await tx.commit();                   // ConflictError → re-run the body
 *
 * Reads are SNAPSHOT reads per shard: the first read against a shard pins
 * that shard's revision, and every later read against the same shard
 * executes at the pinned revision. A pin that ages past the MVCC horizon
 * throws TxTooOldError; restart the transaction (runTx automates this).
 */
export class Tx {
  /** @param {Databox} client */
  constructor(client) {
    /** @private */
    this._c = client;
    /** @private */
    this._reads = {};
    /** @private */
    this._writes = [];
    // cache provides read-your-writes inside the transaction.
    /** @private */
    this._cache = new Map();
    // pins: shard group ID → the shard revision this tx reads at.
    /** @private */
    this._pins = new Map();
  }

  /** Per-shard read versions pinned so far (diagnostics). */
  readVersions() {
    return new Map(this._pins);
  }

  /** @private */
  _pinsParam() {
    return [...this._pins].map(([gid, rev]) => `${gid}:${rev}`).join(",");
  }

  /** @private */
  _pin(gid, shardRev) {
    if (gid > 0 && !this._pins.has(gid)) this._pins.set(gid, shardRev);
  }

  /** @private */
  static _norm(key) {
    return key.startsWith("/") ? key : "/" + key;
  }

  /** Read through the transaction: staged writes are visible, base reads
   * execute at the shard's pinned read version and record the revision
   * seen. null = no such key.
   * @param {string} key @returns {Promise<Uint8Array|null>} */
  async get(key) {
    key = Tx._norm(key);
    if (this._cache.has(key)) return this._cache.get(key) ?? null;
    const q = new URLSearchParams({ tx: "1" });
    if (this._pins.size > 0) q.set("pins", this._pinsParam());
    const out = await this._c._do("GET", "/api/v1/kv" + escapeKey(key) + "?" + q);
    this._pin(out.gid ?? 0, out.shard_rev ?? 0);
    if (out.found) {
      this._reads[key] = out.rev;
      return b64decode(out.value);
    }
    this._reads[key] = 0; // "did not exist" is also a read to validate
    return null;
  }

  /** Scan a prefix at the transaction's snapshot. Every returned key joins
   * the read set; keys absent from the result are NOT validated (phantom
   * inserts do not conflict). Staged writes are not merged in.
   * @param {string} prefix @param {{cursor?: string, limit?: number}} [opts]
   * @returns {Promise<{entries: KVEntry[], nextCursor: string}>} */
  async list(prefix, opts = {}) {
    const q = new URLSearchParams({
      prefix,
      cursor: opts.cursor ?? "",
      pins: this._pinsParam(), // always sent (even empty): selects the versioned scan
    });
    if (opts.limit) q.set("limit", String(opts.limit));
    const out = await this._c._do("GET", "/api/v1/list?" + q);
    for (const [gid, rev] of Object.entries(out.shard_revs ?? {})) {
      this._pin(Number(gid), rev);
    }
    const entries = (out.entries ?? []).map(parseEntry);
    for (const e of entries) {
      if (!this._cache.has(e.key)) this._reads[e.key] = e.rev;
    }
    return { entries, nextCursor: out.next_cursor ?? "" };
  }

  /** Stage a write. @param {string} key @param {Uint8Array|string} value */
  set(key, value) {
    key = Tx._norm(key);
    const v = toBytes(value);
    this._cache.set(key, v);
    this._writes.push({ key, value: b64encode(v) });
  }

  /** Stage a deletion. @param {string} key */
  delete(key) {
    key = Tx._norm(key);
    this._cache.set(key, null);
    this._writes.push({ key, delete: true });
  }

  /** Submit the transaction. ConflictError means another writer won;
   * re-run the transaction body and commit again (or use runTx). */
  async commit() {
    if (this._writes.length === 0) return; // read-only txs validate trivially
    // Commit must NOT ride the generic retry loop: a Conflict result is a
    // real answer, not a transient fault.
    await this._c._do("POST", "/api/v1/tx/commit", { reads: this._reads, writes: this._writes }, 1);
  }
}

/**
 * A TTL'd lock kept alive by a refresh timer (period = ttl/3). Databox has
 * no separate lease subsystem — a lease IS a TTL'd lock plus keepalive,
 * with the fencing token as the guard against stale holders.
 */
export class Lease {
  /**
   * @param {Databox} client @param {string} resource
   * @param {"exclusive"|"shared"} mode @param {number} ttlMs
   * @param {string} handle @param {{fencing: number, holder: string}} grant
   * @param {((err: unknown) => void)=} onLost
   */
  constructor(client, resource, mode, ttlMs, handle, grant, onLost) {
    /** @private */
    this._c = client;
    this.resource = resource;
    this.mode = mode;
    this.ttlMs = ttlMs;
    this.handle = handle;
    this.fencing = grant.fencing;
    this.holder = grant.holder;
    this.alive = true;
    /** @private */
    this._onLost = onLost;
    /** @private */
    this._timer = setInterval(() => void this._refresh(), Math.max(ttlMs / 3, 50));
    this._timer.unref?.();
  }

  /** @private */
  async _refresh() {
    try {
      const grant = await this._c.lockAcquire(this.resource, {
        mode: this.mode,
        ttlMs: this.ttlMs,
        handle: this.handle,
      });
      this.fencing = grant.fencing;
    } catch (err) {
      clearInterval(this._timer);
      this.alive = false;
      this._onLost?.(err);
    }
  }

  /** Stop refreshing and release the lock. */
  async release() {
    clearInterval(this._timer);
    this.alive = false;
    try {
      await this._c.lockRelease(this.resource, this.handle);
    } catch (err) {
      if (!(err instanceof ConflictError)) throw err; // expired/forced is fine
    }
  }
}
