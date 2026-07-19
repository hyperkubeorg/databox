# SQL Layer

You will run the PostgreSQL-wire SQL layer and use its dialect end to end.
The wire protocol is PostgreSQL's — any psql/pg driver connects — but the
language is **not** pg's or MySQL's: it is databox's own dialect (cloned
from chai's grammar). This page is the full walkthrough of what's
possible. Every example below runs verbatim.

## Running the gateway

```sh
databox gateway sql --cluster db-node1:8443 --listen :5432
psql "host=localhost port=5432 user=sam sslmode=require"
```

On the `make kind-up` cluster the gateway already runs; reach it with
`make port-forward-sql` (localhost:5432) or NodePort 30432 (`make
relay-sql` is the streaming-safe alternative —
[kindrelay](../admin/kindrelay.md)).

No psql at hand? `databox utils sql` embeds the same engine in-process
over an authenticated cluster connection — no gateway required.
`--endpoint` defaults to the in-cluster API service (`databox:8443`):

```sh
databox utils sql --endpoint db-node1:8443 --user sam
databox utils sql --endpoint db-node1:8443 -e 'SELECT * FROM orders LIMIT 5;'
```

- **Auth**: the pg username/password *is* the databox user (argon2id
  verified). Statements execute with that user's grants.
- **Databases**: the session's database defaults to the username (`-d`
  overrides it in `utils sql`, the `dbname` parameter in pg clients).
- **Storage mapping**: rows live under `/sql/<database>/<table>/`, indexes
  under `/sql/<database>/_idx/<table>/<index>/`. Tables are key prefixes,
  so **grants work at table granularity**: `databox grant add sam allow
  /sql/app/orders list,read,write`.
- **Extended protocol**: Parse/Bind/Describe/Execute with `$N` parameters
  works — pgx and psycopg connect without simple-query fallbacks.

## Creating tables

```sql
CREATE TABLE products (
  id       INTEGER PRIMARY KEY,
  sku      TEXT NOT NULL UNIQUE,
  name     TEXT NOT NULL,
  price    DOUBLE PRECISION DEFAULT 0.0,
  in_stock BOOLEAN DEFAULT true,
  added    TIMESTAMP,
  photo    BYTEA,
  CHECK (price >= 0)
);
```

Constraints: column-level `PRIMARY KEY`, `NOT NULL`, `UNIQUE`, `DEFAULT
<expr>` (validated at CREATE time; `DEFAULT now()` works), plus
table-level `PRIMARY KEY (a, b)`, `UNIQUE (a, b)`, and `CHECK (expr)` (a
`CONSTRAINT <name>` prefix parses, but checks are stored under chai-style
auto names: `<table>_check`, `<table>_check1`, …). `IF NOT EXISTS` /
`IF EXISTS` work on CREATE/DROP.

A table without a declared primary key gets an invisible auto-assigned
rowid — fine for append-and-scan tables:

```sql
CREATE TABLE archive (name TEXT, price DOUBLE PRECISION);
```

The primary key IS the row's storage key: point lookups and PK ranges are
the fastest access path (see [How queries execute](#how-queries-execute)).

### Auto-increment and UUID keys

`SERIAL` (pg spelling) or `INTEGER AUTO_INCREMENT` (MySQL spelling, also
`AUTOINCREMENT`) declares an auto-assigned integer primary key. The column
must be the single-column primary key. Omitted or `NULL` values draw the
next counter value — assigned inside the insert's transaction, so
concurrent inserts never collide — and an explicit value ratchets the
counter past itself, so later auto values never reuse it:

```sql
CREATE TABLE notes (id SERIAL PRIMARY KEY, body TEXT);
INSERT INTO notes (body) VALUES ('first'), ('second') RETURNING id;
--  id
--  1
--  2
INSERT INTO notes (id, body) VALUES (100, 'jump');
INSERT INTO notes (body) VALUES ('third') RETURNING id;   -- 101
```

`UUID` columns store canonical lowercase `8-4-4-4-12` text, validated and
normalized on every write (uppercase or un-hyphenated input is accepted
and canonicalized; anything else is `invalid UUID`). The idiomatic UUID
primary key pairs the type with pg's generator function:

```sql
CREATE TABLE items (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), name TEXT);
INSERT INTO items (name) VALUES ('widget') RETURNING id;
--  id
--  f5f6f3e5-a6d5-4dcd-b491-3871b2ea1179
```

Because stored values are canonical, compare with the canonical lowercase
hyphenated form in queries — query literals are **not** normalized
(`WHERE id = '123E4567…'` without hyphens will not match).

## Altering tables

Four operations (an extension — chai has no ALTER). Rows always store
every schema column, so these are metadata-plus-rewrite: ADD backfills
every existing row (DEFAULT per row — `now()`/`gen_random_uuid()` give
each row a fresh value — else NULL), DROP and RENAME rewrite rows, and
RENAME TO moves the whole table and its indexes. On a huge table these
statements do real work; they are not lazy migrations.

```sql
ALTER TABLE products ADD COLUMN color TEXT DEFAULT 'natural';
ALTER TABLE products DROP COLUMN color;
ALTER TABLE products RENAME COLUMN sku TO code;   -- indexes and CHECKs follow
ALTER TABLE products RENAME TO catalog;
```

Guards: a `NOT NULL` add needs a `DEFAULT` unless the table is empty;
ADD COLUMN takes only `NOT NULL`/`DEFAULT` (keys and uniqueness are
`CREATE INDEX`'s job, auto-increment is CREATE-time only); dropping a
column that the primary key, an index, or a CHECK uses is refused with
a message naming the dependency.

## Data types

Eight declared types over seven storage types (UUID stores as canonical
TEXT). Type names are aliases from both pg and MySQL traditions; each
collapses onto one storage type:

| Type | Aliases | Literals / notes |
|------|---------|------------------|
| `INTEGER` | `INT`, `TINYINT`, `SMALLINT`, `MEDIUMINT`, `INT2`, `INT4`, `BIGINT`, `INT8`; `SERIAL`/`BIGSERIAL` = INTEGER + auto-increment | 64-bit; `typeof(1)` → `integer` |
| `DOUBLE PRECISION` | `DOUBLE`, `FLOAT8`, `REAL` | `typeof(1.5)` → `double precision`; INTEGER 1 and DOUBLE 1.0 stay distinguishable |
| `TEXT` | `CLOB`, `VARCHAR(n)`, `CHARACTER`, `CHAR(n)` — length is parsed and ignored | `'single quotes'`, `''` escapes a quote |
| `BOOLEAN` | `BOOL` | `true`/`false`; renders `t`/`f` in results |
| `BYTEA` | `BLOB`, `BYTES` | hex literals: `'\x89504e47'` is 4 bytes; `'YXNkaW5l'::BYTEA` decodes base64 |
| `TIMESTAMP` | `TIMESTAMPTZ`, `DATETIME` | string form `'2026-07-01 10:00:00'` coerces wherever a timestamp is expected; `now()` |
| `UUID` | — | stores as canonical-form TEXT, validated on write; `gen_random_uuid()` — see [Auto-increment and UUID keys](#auto-increment-and-uuid-keys) |
| `VECTOR(n)` | — | pgvector-style extension, dimension mandatory — see [Vector search](#vector-search-pgvector-compatible) |

`NULL` is its own thing: `x = NULL` is never true — use `IS NULL` /
`IS NOT NULL`. Sorting uses one total order across types
(`NULL < BOOLEAN < numbers < TEXT < BYTEA < TIMESTAMP`), so heterogeneous
columns never fail to sort.

Casts: pg's `::` shorthand or `CAST(x AS type)`:

```sql
SELECT 249.99::INTEGER;            -- 249
SELECT typeof('\xdeadbeef');       -- bytea
```

## Inserting

Multi-row `VALUES`, a `SELECT` source, `RETURNING`, and conflict handling:

```sql
INSERT INTO products (id, sku, name, price, added) VALUES
  (1, 'CH-001', 'Walnut Chair', 249.99, '2026-07-01 10:00:00'),
  (2, 'TB-104', 'Oak Table', 899.00, '2026-07-02 09:30:00'),
  (3, 'LA-220', 'Brass Lamp', 79.50, '2026-07-03 14:15:00');

INSERT INTO products (id, sku, name, price)
  VALUES (4, 'RG-330', 'Wool Rug', 129.00)
  RETURNING id, name, price;
--  id | name     | price
--  4  | Wool Rug | 129

-- keep the existing row:
INSERT INTO products (id, sku, name) VALUES (4, 'RG-330', 'Wool Rug')
  ON CONFLICT DO NOTHING;                   -- INSERT 0 0

-- or overwrite it:
INSERT INTO products (id, sku, name, price)
  VALUES (4, 'RG-330', 'Wool Rug (large)', 159.00)
  ON CONFLICT DO REPLACE;                   -- INSERT 0 1

INSERT INTO archive SELECT name, price FROM products WHERE price < 200;
```

`RETURNING` exists on INSERT only (not UPDATE/DELETE). A multi-row INSERT
is atomic: all rows and all index entries commit in one databox
transaction, or none do.

## Querying

```sql
SELECT name, price FROM products
  WHERE price BETWEEN 100 AND 900
  ORDER BY price DESC;
--  name             | price
--  Oak Table        | 899
--  Walnut Chair     | 249.99
--  Wool Rug (large) | 159

SELECT sku, name FROM products WHERE name LIKE 'W%' OR sku IN ('LA-220');
SELECT name FROM products WHERE added IS NULL;
SELECT name FROM products WHERE added > '2026-07-01 12:00:00';
```

`WHERE` supports `=`, `!=`/`<>`, `<`, `<=`, `>`, `>=`, `AND`/`OR`/`NOT`,
`LIKE`/`NOT LIKE` (`%` and `_` wildcards), `IN (...)`, `BETWEEN`,
`IS [NOT] NULL`. Projections take expressions and aliases; `||`
concatenates:

```sql
SELECT name || ' — $' || price AS label FROM products
  ORDER BY id LIMIT 2 OFFSET 1;
--  label
--  Oak Table — $899
--  Brass Lamp — $79.5
```

`DISTINCT`, `UNION` (deduplicates) and `UNION ALL` (doesn't) work:

```sql
SELECT name FROM products WHERE price > 500
UNION ALL
SELECT name FROM archive
ORDER BY name;
```

Scalar functions: `lower`, `upper`, `trim`/`ltrim`/`rtrim`, `length`,
`abs`, `coalesce`, `typeof`, `now`, `gen_random_uuid`, plus the
[vector functions](#vector-search-pgvector-compatible):

```sql
SELECT upper(name) AS shout, length(name) AS len FROM products WHERE id = 1;
SELECT coalesce(added, '1970-01-01 00:00:00') FROM products WHERE id = 4;
```

## Joins

The full set, chained left-to-right (an extension — chai has none):
`[INNER] JOIN … ON|USING`, `LEFT/RIGHT/FULL [OUTER] JOIN … ON|USING`,
`CROSS JOIN`, `NATURAL [INNER|LEFT|RIGHT|FULL] JOIN`, and old-style comma
joins (`FROM a, b WHERE …`). Qualified names (`u.id`), table aliases, and
self-joins (`FROM emp a JOIN emp b ON …`) all work; a column name that
exists in exactly one table may stay bare, and referencing an ambiguous
bare name is an error that tells you to qualify it.

```sql
SELECT u.name, o.amount
  FROM users u JOIN orders o ON u.id = o.uid
  ORDER BY o.amount;

SELECT u.name, count(*) AS orders, sum(o.amount) AS total
  FROM users u LEFT JOIN orders o ON u.id = o.uid
  GROUP BY u.name;

SELECT u.name FROM users u
  LEFT JOIN orders o ON u.id = o.uid
  WHERE o.id IS NULL;          -- users with no orders

SELECT dept_id, name, dept
  FROM emp FULL JOIN dept USING (dept_id);
--  dept_id | name | dept
--  10      | ann  | eng
--  30      | cid  | NULL     ← emp side only
--  40      | NULL | hr       ← dept side only
```

`USING (col, …)` joins on the named columns and gives them pg semantics:
the bare name resolves to the coalesced value (left side when present,
right otherwise — so it is non-NULL on both halves of a FULL join), and
`SELECT *` shows each USING column once, first. `NATURAL` computes the
USING set from the column names the sides share (no common columns
degrades to CROSS, as in pg).

Execution: every table scans at the statement's snapshot, then joins fold
in memory — a hash join when the join keys are known (USING/NATURAL, or
an ON clause that is a pure conjunction of cross-side equalities), a
nested loop for any other predicate. `NULL` join keys never match
(standard SQL). `SELECT *` on a join expands to qualified labels. The
join words stay unreserved — tables and columns named `join`/`left`/
`using`/etc. keep working — at one cost: a bare FROM alias cannot be one
of `join`, `inner`, `left`, `right`, `full`, `cross`, `outer`,
`natural`, `using`, `lateral` (use `AS`). Parenthesized join groups and
LATERAL subqueries are covered in [Subqueries](#subqueries); joins in
UPDATE/DELETE are not supported.

## Subqueries

Three expression forms, each executed once per statement:

```sql
SELECT name FROM users WHERE id = (SELECT max(uid) FROM orders);   -- scalar
SELECT name FROM users WHERE id IN (SELECT uid FROM orders);       -- IN
SELECT name FROM users WHERE EXISTS (SELECT 1 FROM orders WHERE amount > 90);
UPDATE orders SET amount = (SELECT max(amount) FROM orders) WHERE id = 12;
```

A scalar subquery must return one column and at most one row (zero rows
reads as NULL). They work anywhere an expression does — WHERE, the
projection, DML values — except stored DDL expressions (DEFAULT/CHECK
refuse them). **Correlated** subqueries in expressions are not supported;
the per-row form has a first-class spelling instead — LATERAL:

```sql
-- top order per user (the canonical LATERAL shape)
SELECT u.name, top.amount
  FROM users u
  LEFT JOIN LATERAL (SELECT amount FROM orders
                      WHERE uid = u.id
                      ORDER BY amount DESC LIMIT 1) top ON true;
```

A LATERAL subquery re-evaluates per row of the sources to its left, with
their columns in scope (aggregates inside work — a correlated `count(*)`
is one `LEFT JOIN LATERAL` away). It pairs with `JOIN … ON` and
`LEFT JOIN … ON` (`ON true` is the usual predicate); its projection must
list columns explicitly (no `*`).

**Derived tables** — subqueries in FROM — join like any other source and
need an alias:

```sql
SELECT u.name, t.total
  FROM users u
  JOIN (SELECT uid, sum(amount) AS total FROM orders GROUP BY uid) t
    ON u.id = t.uid;
```

**Parenthesized join groups** control join order — the difference is real
with outer joins:

```sql
SELECT u.name, o.amount FROM users u
  LEFT JOIN (orders o JOIN regions r ON o.uid = r.uid) ON u.id = o.uid;
-- users survive even when the inner (orders⋈regions) join has no match
```

## Aggregates

`count(*)`, `count(col)` (non-NULL), `sum`, `avg`, `min`, `max`, with
`GROUP BY`:

```sql
SELECT count(*), sum(price), avg(price), min(price), max(price) FROM products;

SELECT in_stock, count(*) AS n, avg(price) AS avg_price
  FROM products GROUP BY in_stock;
--  in_stock | n | avg_price
--  t        | 4 | 346.8725
```

There is no `HAVING` — filter before grouping with `WHERE`, or filter the
grouped result in your application. Aggregates work over
[joins](#joins) too.

## Updating and deleting

```sql
UPDATE products SET price = price * 0.9 WHERE sku = 'TB-104';
DELETE FROM products WHERE id = 3;
```

`SET` takes expressions over the row's current values. Each statement is
atomic — every touched row and index entry commits together.

## Indexes and how queries execute

```sql
CREATE INDEX ON products (price);                 -- auto-named products_price_idx
CREATE UNIQUE INDEX products_name ON products (name);
CREATE INDEX ON products (in_stock, price);       -- composite
DROP INDEX products_name;
```

A planner picks the access path per statement — primary-key point lookup
or range, secondary-index range scan, or full table scan as the
always-correct fallback. Plans only narrow where the executor reads; the
full `WHERE` clause is always re-applied. So indexes are a performance
tool, never a correctness risk: point lookups and ranges on the PK are
fastest, an index on a filtered column turns a table scan into a range
scan, and everything else still works unindexed.

Statements run at snapshot isolation; scans map to ranged `List` calls
with page-at-a-time prefetching.

## Vector search (pgvector-compatible)

`VECTOR(n)` columns hold a fixed-dimension float32 array (n from 1 to
16000). The dimension is mandatory and enforced on every write
(`expected 3 dimensions, got 2`). Literals use pgvector's text form —
the string `'[1, 2.5, -3]'` coerces to a vector wherever one is expected
(INSERT, operator operand, function argument, `$N` text parameter).
Output is the canonical `[1,2.5,-3]`, served as text (OID 25) on the wire.

```sql
CREATE TABLE docs (id INTEGER PRIMARY KEY, title TEXT, embedding VECTOR(3));
INSERT INTO docs VALUES
  (1, 'intro', '[0.1, 0.9, 0.0]'),
  (2, 'setup', '[0.8, 0.1, 0.1]'),
  (3, 'faq',   '[0.4, 0.5, 0.1]');

SELECT title, embedding <-> '[0.9, 0.05, 0.05]' AS dist
  FROM docs ORDER BY embedding <-> '[0.9, 0.05, 0.05]' LIMIT 2;
--  title | dist
--  setup | 0.12247445854730495
--  faq   | 0.6745368556288323
```

| Operator | Returns | pgvector equivalent |
|----------|---------|---------------------|
| `a <-> b` | L2 (Euclidean) distance | `<->` |
| `a <#> b` | **negative** inner product | `<#>` |
| `a <=> b` | cosine distance | `<=>` |

Operators return `DOUBLE PRECISION` and bind just above comparison, so
`a <-> b < 0.5` means `(a <-> b) < 0.5`. Cosine distance of a
zero-magnitude vector is an error (pgvector returns NaN; an error is
strictly more useful). Dimension mismatch between operands is an error.

Functions: `vector_dims(v)` → integer, `l2_distance(a,b)`,
`inner_product(a,b)` (positive, unlike `<#>`), `cosine_distance(a,b)`,
`l2_norm(v)` → double precision.

`ORDER BY embedding <-> $1 LIMIT k` (k+offset ≤ 10000) executes as an
exact single-pass KNN scan through a bounded top-k heap — no full-table
sort, and identical results to one. There are **no ANN indexes**
(HNSW/IVFFlat) in v1: every search is exact, and vector columns cannot be
a PRIMARY KEY or appear in `CREATE INDEX`, `GROUP BY`, `DISTINCT` or
`UNION` (`UNION ALL` is fine).

## Introspection

Dialect extensions (none of these words are reserved — they only mean
anything at statement start, so columns named `show` keep working):

```sql
SHOW TABLES;               -- tables in the current database
SHOW DATABASES;            -- databases your grants let you see
SHOW COLUMNS FROM products;  DESCRIBE products;  DESC products;
--  column   | type             | nullable | key | default | extra
--  id       | INTEGER          | NO       | PRI | NULL    |
--  sku      | TEXT             | NO       | UNI | NULL    |
--  price    | DOUBLE PRECISION | YES      |     | 0.0     |
--  ...                                    (extra: auto_increment)
SHOW INDEXES FROM products;  -- PRIMARY pseudo-row + secondary indexes
SHOW CREATE TABLE products;  -- canonical DDL; round-trips through the engine
```

## Transactions

Each statement is fully atomic: all rows plus all index maintenance
commit in one databox transaction at snapshot isolation, with automatic
retry on write conflict. Multi-statement transactions are **not**
supported — `BEGIN`/`COMMIT`/`ROLLBACK` parse but are autocommit no-ops
for driver compatibility, and `ROLLBACK` undoes nothing.

## Not in this dialect

The explicit v1 boundary, with the errors you'll see:

| Statement | Result |
|-----------|--------|
| `HAVING` | `expected EOF, got having` |
| Correlated subqueries in expressions | error with a hint — rewrite as `[LEFT] JOIN LATERAL` |
| Sequences | not parsed — `SERIAL`/`AUTO_INCREMENT` cover the id case |
| `EXPLAIN` | not parsed |
| `information_schema` / `pg_catalog` / `__chai_catalog` | absent — use `SHOW`/`DESCRIBE` |
| SCRAM auth | password auth only |

Conformance is measured against chai's own `sqltests/` corpus, cloned into
the test suite: **548 cases pass, 0 fail, 201 are skipped** via an explicit
ledger with reasons (sequences, `ALTER TABLE`, `EXPLAIN`, `__chai_catalog`).
Assertions are never weakened to pass; the tally prints on every
`go test ./pkg/service/sql/`.
