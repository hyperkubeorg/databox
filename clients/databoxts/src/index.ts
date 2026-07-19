/**
 * databoxts — TypeScript client for the databox database (Bun runtime).
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
  /** Server error code — the leading token of the message, e.g. "Conflict". */
  code: string;
  /** HTTP status (0 = transport failure). */
  status: number;
  constructor(message: string, code = "", status = 0) {
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

function typedError(message: string, status: number): DataboxError {
  const code = message.split(":", 1)[0]!.trim();
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

function retryable(err: unknown): boolean {
  // Network errors, 409, and 503 retry per the documented convention.
  if (err instanceof DataboxError) return err.status === 409 || err.status === 503;
  return !(err instanceof Error && err.name === "AbortError");
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export interface KVEntry {
  key: string;
  value: Uint8Array;
  rev: number;
  blob: boolean;
}

/** One watch stream event. type is "put" or "delete" (deletes carry no value). */
export interface WatchEvent {
  rev: number;
  type: "put" | "delete";
  key: string;
  value: Uint8Array;
  blob: boolean;
}

export interface BlobStat {
  size: number;
  contentType: string;
  sha256: string;
}

export interface BlobResult {
  rev: number;
  size: number;
  sha256: string;
  mode: string; // "replica" | "ec"
  composite: boolean;
}

export interface LockGrant {
  fencing: number;
  holder: string;
}

export interface DataboxOptions {
  /** "host:port" — https is implied and required. */
  endpoint: string;
  /** Pre-set a session token (otherwise call login). */
  token?: string;
  /** PEM CA certificate(s) verifying the cluster (production). */
  ca?: string;
  /** Skip certificate verification — dev only. */
  insecure?: boolean;
  /** Attempt budget for retryable errors (default 5). */
  retries?: number;
}

export type Bytes = Uint8Array | string;

/** Body types accepted for blob uploads. */
export type BlobBody = Uint8Array | Blob | ReadableStream<Uint8Array>;

function toBytes(v: Bytes): Uint8Array {
  return typeof v === "string" ? new TextEncoder().encode(v) : v;
}

function b64encode(v: Uint8Array): string {
  return Buffer.from(v).toString("base64");
}

function b64decode(s: string | undefined): Uint8Array {
  return s ? new Uint8Array(Buffer.from(s, "base64")) : new Uint8Array(0);
}

/** Render a user key ("/a/b c") as a URL path suffix: slashes stay
 * separators, everything else is escaped. */
function escapeKey(key: string): string {
  if (!key.startsWith("/")) key = "/" + key;
  return key.split("/").map(encodeURIComponent).join("/");
}

function escapeResource(resource: string): string {
  // Lock resources are plain names (no leading slash), unlike keyspace keys.
  return resource.split("/").map(encodeURIComponent).join("/");
}

function parseEntry(e: Record<string, unknown>): KVEntry {
  return {
    key: e.key as string,
    value: b64decode(e.value as string | undefined),
    rev: e.rev as number,
    blob: Boolean(e.blob),
  };
}

/** Client for one databox cluster endpoint. */
export class Databox {
  readonly base: string;
  token: string;
  retries: number;
  private tls: Record<string, unknown> | undefined;

  constructor(opts: DataboxOptions) {
    if (!opts.endpoint) throw new Error("endpoint required");
    this.base = "https://" + opts.endpoint;
    this.token = opts.token ?? "";
    this.retries = opts.retries ?? 5;
    if (opts.ca) this.tls = { ca: opts.ca };
    else if (opts.insecure) this.tls = { rejectUnauthorized: false };
  }

  private headers(json = false): Record<string, string> {
    const h: Record<string, string> = {};
    if (this.token) h.Authorization = "Bearer " + this.token;
    if (json) h["Content-Type"] = "application/json";
    return h;
  }

  /** One fetch with Bun's TLS options applied. */
  private fetch(path: string, init: RequestInit & { headers?: Record<string, string> }) {
    return fetch(this.base + path, { ...init, tls: this.tls } as RequestInit);
  }

  private async once(method: string, path: string, body?: unknown): Promise<any> {
    const resp = await this.fetch(path, {
      method,
      headers: this.headers(body !== undefined),
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (resp.status === 200) {
      const text = await resp.text();
      return text ? JSON.parse(text) : undefined;
    }
    let msg = "";
    try {
      msg = ((await resp.json()) as { error?: string }).error ?? "";
    } catch {}
    throw typedError(msg || `HTTP ${resp.status}`, resp.status);
  }

  private async do(method: string, path: string, body?: unknown, retries?: number): Promise<any> {
    const attempts = Math.max(1, retries ?? this.retries);
    let backoff = BACKOFF_START_MS;
    let last: unknown;
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (attempt > 0) {
        await sleep(backoff + Math.random() * (backoff / 2));
        backoff = Math.min(backoff * 2, BACKOFF_CAP_MS);
      }
      try {
        return await this.once(method, path, body);
      } catch (err) {
        last = err;
        if (!retryable(err)) throw err;
      }
    }
    throw last;
  }

  /** Arbitrary API call — admin endpoints (users, grants, cluster status,
   * policies) without a dedicated method for each. */
  raw(method: string, path: string, body?: unknown): Promise<any> {
    return this.do(method, path, body);
  }

  // --- auth -----------------------------------------------------------

  async login(username: string, password: string): Promise<void> {
    const out = await this.do("POST", "/api/v1/auth/login", { username, password });
    this.token = out.token;
  }

  async logout(): Promise<void> {
    await this.do("POST", "/api/v1/auth/logout");
    this.token = "";
  }

  // --- kv -------------------------------------------------------------

  /** Fetch one key (linearizable). null means no such key. */
  async get(key: string): Promise<KVEntry | null> {
    try {
      const out = await this.do("GET", "/api/v1/kv" + escapeKey(key));
      return parseEntry(out);
    } catch (err) {
      if (err instanceof NotFoundError) return null;
      throw err;
    }
  }

  /** Write one key, returning its new revision. */
  async set(key: string, value: Bytes): Promise<number> {
    const out = await this.do("PUT", "/api/v1/kv" + escapeKey(key), {
      value: b64encode(toBytes(value)),
    });
    return out.rev;
  }

  async delete(key: string): Promise<void> {
    await this.do("DELETE", "/api/v1/kv" + escapeKey(key));
  }

  /** Remove [start, end). */
  async deleteRange(start: string, end: string): Promise<void> {
    await this.do("POST", "/api/v1/delete-range", { start, end });
  }

  /** One page of keys under prefix. Empty nextCursor = end. */
  async list(
    prefix = "/",
    opts: { cursor?: string; limit?: number } = {},
  ): Promise<{ entries: KVEntry[]; nextCursor: string }> {
    const q = new URLSearchParams({ prefix, cursor: opts.cursor ?? "" });
    if (opts.limit) q.set("limit", String(opts.limit));
    const out = await this.do("GET", "/api/v1/list?" + q);
    return {
      entries: ((out.entries ?? []) as Record<string, unknown>[]).map(parseEntry),
      nextCursor: out.next_cursor ?? "",
    };
  }

  /** Iterate every key under prefix, paging transparently. */
  async *iter(prefix = "/", pageSize = 1000): AsyncGenerator<KVEntry> {
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
   */
  async *watch(
    prefix: string,
    opts: { fromRev?: number; signal?: AbortSignal } = {},
  ): AsyncGenerator<WatchEvent> {
    const q = new URLSearchParams({ prefix });
    if (opts.fromRev) q.set("from_revision", String(opts.fromRev));
    const resp = await this.fetch("/api/v1/watch?" + q, {
      method: "GET",
      headers: this.headers(),
      signal: opts.signal,
    });
    if (resp.status !== 200) {
      let msg = "";
      try {
        msg = ((await resp.json()) as { error?: string }).error ?? "";
      } catch {}
      throw typedError(msg || `HTTP ${resp.status}`, resp.status);
    }
    const reader = resp.body!.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        buf += decoder.decode(value, { stream: true });
        let nl: number;
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
   * OCC-validated at commit). */
  tx(): Tx {
    return new Tx(this);
  }

  /**
   * Run fn inside a transaction and commit, restarting the whole
   * transaction (fresh reads) with backoff on commit Conflict or
   * TxTooOld. Any other error rejects immediately.
   */
  async runTx(fn: (tx: Tx) => Promise<void> | void): Promise<void> {
    const attempts = Math.max(1, this.retries);
    let backoff = BACKOFF_START_MS;
    let last: unknown;
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
   * (no leading slash), unlike keyspace keys. */
  async lockAcquire(
    resource: string,
    opts: { mode?: "exclusive" | "shared"; ttlMs?: number; handle?: string } = {},
  ): Promise<LockGrant> {
    const out = await this.do("POST", "/api/v1/locks/acquire", {
      resource,
      mode: opts.mode ?? "exclusive",
      ttl_ms: opts.ttlMs ?? 30_000,
      handle: opts.handle ?? "",
    });
    return { fencing: out.fencing, holder: out.holder ?? "" };
  }

  async lockRelease(resource: string, handle = ""): Promise<void> {
    await this.do("POST", "/api/v1/locks/release", { resource, handle });
  }

  /** {locked: boolean, state?: ...} for a resource. */
  lockStatus(resource: string): Promise<{ locked: boolean; state?: unknown }> {
    return this.do("GET", "/api/v1/locks/" + escapeResource(resource));
  }

  /**
   * Acquire a lock and keep it alive with a timer, refreshing at ttl/3.
   * Resolves once acquired (with the retry budget); call release() when
   * done. The fencing token fences out stale holders: pass it to whatever
   * the lease guards and reject smaller tokens there.
   */
  async lease(
    resource: string,
    opts: {
      ttlMs?: number;
      mode?: "exclusive" | "shared";
      handle?: string;
      onLost?: (err: unknown) => void;
    } = {},
  ): Promise<Lease> {
    const ttlMs = opts.ttlMs ?? 30_000;
    const grant = await this.lockAcquire(resource, {
      mode: opts.mode,
      ttlMs,
      handle: opts.handle,
    });
    return new Lease(this, resource, opts.mode ?? "exclusive", ttlMs, opts.handle ?? "", grant, opts.onLost);
  }

  // --- blobs ----------------------------------------------------------

  private async blobRequest(
    method: string,
    key: string,
    body?: BlobBody,
    contentType = "",
    query?: Record<string, string>,
  ): Promise<Response> {
    let path = "/api/v1/blobs" + escapeKey(key);
    if (query) path += "?" + new URLSearchParams(query);
    const headers = this.headers();
    if (contentType) headers["Content-Type"] = contentType;
    return this.fetch(path, { method, headers, body } as RequestInit & { headers: Record<string, string> });
  }

  private static async blobError(resp: Response): Promise<DataboxError> {
    let msg = "";
    try {
      msg = ((await resp.json()) as { error?: string }).error ?? "";
    } catch {}
    return typedError(msg || `HTTP ${resp.status}`, resp.status);
  }

  private static async blobResult(resp: Response): Promise<BlobResult> {
    const out = (await resp.json()) as Record<string, unknown>;
    return {
      rev: out.rev as number,
      size: out.size as number,
      sha256: (out.sha256 as string) ?? "",
      mode: (out.mode as string) ?? "",
      composite: Boolean(out.composite),
    };
  }

  /** Store a blob (raw byte stream; no size cap like KV values). */
  async putBlob(key: string, data: Bytes | Blob | ReadableStream<Uint8Array>, contentType = ""): Promise<BlobResult> {
    const body = typeof data === "string" ? toBytes(data) : data;
    const resp = await this.blobRequest("PUT", key, body, contentType);
    if (resp.status !== 200) throw await Databox.blobError(resp);
    return Databox.blobResult(resp);
  }

  /** Extend a blob. ConflictError means a concurrent append won — retry
   * (the failed attempt's data never became visible). */
  async appendBlob(key: string, data: Bytes | Blob): Promise<BlobResult> {
    const body = typeof data === "string" ? toBytes(data) : data;
    const resp = await this.blobRequest("PATCH", key, body);
    if (resp.status !== 200) throw await Databox.blobError(resp);
    return Databox.blobResult(resp);
  }

  async getBlob(key: string): Promise<Uint8Array> {
    const resp = await this.blobRequest("GET", key);
    if (resp.status !== 200) throw await Databox.blobError(resp);
    return new Uint8Array(await resp.arrayBuffer());
  }

  /** length < 0 = to the end. The server never touches chunks outside the
   * window — the primitive HTTP Range serving builds on. */
  async getBlobRange(key: string, offset: number, length = -1): Promise<Uint8Array> {
    const q: Record<string, string> = { offset: String(offset) };
    if (length >= 0) q.length = String(length);
    const resp = await this.blobRequest("GET", key, undefined, "", q);
    if (resp.status !== 200) throw await Databox.blobError(resp);
    return new Uint8Array(await resp.arrayBuffer());
  }

  /** Stream a blob's bytes without buffering it whole. */
  async streamBlob(key: string): Promise<ReadableStream<Uint8Array>> {
    const resp = await this.blobRequest("GET", key);
    if (resp.status !== 200) throw await Databox.blobError(resp);
    return resp.body!;
  }

  /** Size/content-type/hash without reading data. null = no blob. */
  async statBlob(key: string): Promise<BlobStat | null> {
    const resp = await this.blobRequest("HEAD", key);
    await resp.arrayBuffer(); // drain
    if (resp.status === 404) return null;
    if (resp.status !== 200) throw typedError(`HTTP ${resp.status}`, resp.status);
    return {
      size: Number(resp.headers.get("Content-Length") ?? 0),
      contentType: resp.headers.get("Content-Type") ?? "",
      sha256: resp.headers.get("X-Databox-SHA256") ?? "",
    };
  }

  async deleteBlob(key: string): Promise<void> {
    await this.do("DELETE", "/api/v1/blobs" + escapeKey(key));
  }

  /** Concatenate the blobs at sources, in order, into destination —
   * server-side; no blob data streams through the client. Multi-source
   * destinations carry a composite hash and refuse appendBlob. */
  async spliceBlobs(destination: string, sources: string[], contentType = ""): Promise<BlobResult> {
    const out = await this.do("POST", "/api/v1/blobs-splice", {
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

  // internal access for Tx
  /** @internal */
  _do(method: string, path: string, body?: unknown, retries?: number): Promise<any> {
    return this.do(method, path, body, retries);
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
  private reads: Record<string, number> = {};
  private writes: Array<Record<string, unknown>> = [];
  // cache provides read-your-writes inside the transaction.
  private cache = new Map<string, Uint8Array | null>();
  // pins: shard group ID → the shard revision this tx reads at.
  private pins = new Map<number, number>();

  constructor(private c: Databox) {}

  /** Per-shard read versions pinned so far (diagnostics). */
  readVersions(): Map<number, number> {
    return new Map(this.pins);
  }

  private pinsParam(): string {
    return [...this.pins].map(([gid, rev]) => `${gid}:${rev}`).join(",");
  }

  private pin(gid: number, shardRev: number): void {
    if (gid > 0 && !this.pins.has(gid)) this.pins.set(gid, shardRev);
  }

  private static norm(key: string): string {
    return key.startsWith("/") ? key : "/" + key;
  }

  /** Read through the transaction: staged writes are visible, base reads
   * execute at the shard's pinned read version and record the revision
   * seen. null = no such key. */
  async get(key: string): Promise<Uint8Array | null> {
    key = Tx.norm(key);
    if (this.cache.has(key)) return this.cache.get(key) ?? null;
    const q = new URLSearchParams({ tx: "1" });
    if (this.pins.size > 0) q.set("pins", this.pinsParam());
    const out = await this.c._do("GET", "/api/v1/kv" + escapeKey(key) + "?" + q);
    this.pin(out.gid ?? 0, out.shard_rev ?? 0);
    if (out.found) {
      this.reads[key] = out.rev;
      return b64decode(out.value);
    }
    this.reads[key] = 0; // "did not exist" is also a read to validate
    return null;
  }

  /** Scan a prefix at the transaction's snapshot. Every returned key joins
   * the read set; keys absent from the result are NOT validated (phantom
   * inserts do not conflict). Staged writes are not merged in. */
  async list(
    prefix: string,
    opts: { cursor?: string; limit?: number } = {},
  ): Promise<{ entries: KVEntry[]; nextCursor: string }> {
    const q = new URLSearchParams({
      prefix,
      cursor: opts.cursor ?? "",
      pins: this.pinsParam(), // always sent (even empty): selects the versioned scan
    });
    if (opts.limit) q.set("limit", String(opts.limit));
    const out = await this.c._do("GET", "/api/v1/list?" + q);
    for (const [gid, rev] of Object.entries(out.shard_revs ?? {})) {
      this.pin(Number(gid), rev as number);
    }
    const entries = ((out.entries ?? []) as Record<string, unknown>[]).map(parseEntry);
    for (const e of entries) {
      if (!this.cache.has(e.key)) this.reads[e.key] = e.rev;
    }
    return { entries, nextCursor: out.next_cursor ?? "" };
  }

  /** Stage a write. */
  set(key: string, value: Bytes): void {
    key = Tx.norm(key);
    const v = toBytes(value);
    this.cache.set(key, v);
    this.writes.push({ key, value: b64encode(v) });
  }

  /** Stage a deletion. */
  delete(key: string): void {
    key = Tx.norm(key);
    this.cache.set(key, null);
    this.writes.push({ key, delete: true });
  }

  /** Submit the transaction. ConflictError means another writer won;
   * re-run the transaction body and commit again (or use runTx). */
  async commit(): Promise<void> {
    if (this.writes.length === 0) return; // read-only txs validate trivially
    // Commit must NOT ride the generic retry loop: a Conflict result is a
    // real answer, not a transient fault.
    await this.c._do("POST", "/api/v1/tx/commit", { reads: this.reads, writes: this.writes }, 1);
  }
}

/**
 * A TTL'd lock kept alive by a refresh timer (period = ttl/3). Databox has
 * no separate lease subsystem — a lease IS a TTL'd lock plus keepalive,
 * with the fencing token as the guard against stale holders.
 */
export class Lease {
  fencing: number;
  holder: string;
  alive = true;
  private timer: ReturnType<typeof setInterval>;

  constructor(
    private c: Databox,
    readonly resource: string,
    readonly mode: "exclusive" | "shared",
    readonly ttlMs: number,
    readonly handle: string,
    grant: LockGrant,
    private onLost?: (err: unknown) => void,
  ) {
    this.fencing = grant.fencing;
    this.holder = grant.holder;
    this.timer = setInterval(() => void this.refresh(), Math.max(ttlMs / 3, 50));
    if (typeof this.timer === "object" && "unref" in this.timer) this.timer.unref();
  }

  private async refresh(): Promise<void> {
    try {
      const grant = await this.c.lockAcquire(this.resource, {
        mode: this.mode,
        ttlMs: this.ttlMs,
        handle: this.handle,
      });
      this.fencing = grant.fencing;
    } catch (err) {
      clearInterval(this.timer);
      this.alive = false;
      this.onLost?.(err);
    }
  }

  /** Stop refreshing and release the lock. */
  async release(): Promise<void> {
    clearInterval(this.timer);
    this.alive = false;
    try {
      await this.c.lockRelease(this.resource, this.handle);
    } catch (err) {
      if (!(err instanceof ConflictError)) throw err; // expired/forced is fine
    }
  }
}
