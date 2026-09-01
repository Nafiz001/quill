# quill

A relational database with SQL, written from scratch in Go. Disk-backed,
crash-safe, and it speaks the PostgreSQL wire protocol — so the real `psql`
connects to it:

```console
$ quill -serve :15433 shop.db
quill 0.1.0 listening on 127.0.0.1:15433

$ psql -h localhost -p 15433 -U quill
psql (18.0, server 14.0 (quill 0.1.0))

quill=> SELECT u.name, COUNT(*) AS orders, SUM(o.total) AS spent
quill->   FROM users u JOIN orders o ON u.id = o.user_id
quill->  GROUP BY u.name HAVING SUM(o.total) > 100
quill->  ORDER BY spent DESC;
 name  | orders | spent
-------+--------+--------
 Rahim |      1 |   1500
 Nafiz |      2 | 350.49
(2 rows)
```

No storage library, no parser generator, no SQL engine. Pages, a B+tree, a
lexer, a planner, and a write-ahead log, all written here.

## Why build this

A database is the piece of infrastructure most backend engineers use every day
and have never opened. Building one forces you through the parts that stay
hidden: how rows are laid out in a 4KB block, why an index turns a query from
O(n) to O(log n), what a query planner actually decides, and what "durable"
means when the power can fail between two instructions.

## What works

```sql
CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, city TEXT);
CREATE INDEX idx_city ON users (city);
CREATE UNIQUE INDEX idx_email ON users (email);

SELECT city, COUNT(*), AVG(age) FROM users
 WHERE age BETWEEN 20 AND 40 AND name LIKE 'N%'
 GROUP BY city HAVING COUNT(*) > 1
 ORDER BY COUNT(*) DESC LIMIT 10;

SELECT u.name, o.total FROM users u LEFT JOIN orders o ON u.id = o.user_id;

BEGIN; UPDATE users SET city = 'Dhaka' WHERE id = 1; ROLLBACK;

EXPLAIN SELECT * FROM users WHERE id = 42;
```

| | |
|---|---|
| **Statements** | `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `CREATE TABLE`, `CREATE INDEX`, `DROP TABLE`, `BEGIN`/`COMMIT`/`ROLLBACK`, `EXPLAIN` |
| **Query features** | `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`, `LIMIT`/`OFFSET`, `DISTINCT`, inner and left joins, table aliases |
| **Expressions** | arithmetic, comparison, `AND`/`OR`/`NOT`, `IS NULL`, `IN`, `BETWEEN`, `LIKE`, `\|\|` |
| **Aggregates** | `COUNT`, `SUM`, `AVG`, `MIN`, `MAX`, with `DISTINCT` |
| **Types** | `INTEGER`, `FLOAT`, `TEXT`, `BOOLEAN`, with `NULL` |
| **Constraints** | `PRIMARY KEY`, `NOT NULL`, `UNIQUE` indexes |

## Architecture

```
   psql ──▶ ┌──────────────────┐
            │ wire             │  PostgreSQL frontend/backend protocol
            └────────┬─────────┘
                     ▼
            ┌──────────────────┐
            │ sql              │  lexer → recursive-descent parser → AST
            └────────┬─────────┘
                     ▼
            ┌──────────────────┐
            │ exec             │  planner, then volcano-model operators:
            │                  │  Scan → Filter → Aggregate → Project
            └────────┬─────────┘  → Sort → Limit
                     ▼
            ┌──────────────────┐
            │ catalog          │  schema, itself stored in a B+tree
            └────────┬─────────┘
                     ▼
            ┌──────────────────┐
            │ storage          │  B+tree over slotted 4KB pages,
            │                  │  buffer pool with LRU + pin counts,
            └────────┬─────────┘  write-ahead log
                     ▼
                  one file
```

### Decisions worth explaining

**A B+tree, because fanout beats depth.** A binary tree over a million rows is
twenty pointer chases, and on disk each one is a separate read. A B+tree fans
out by hundreds per node, so the same million rows sit three levels deep.
Measured here: **100,000 rows produce a tree of height 3.** Values live only in
leaves and the leaves are linked, so a range scan is one descent plus a linear
walk rather than a fresh descent per row.

**Slotted pages, so a row can move without anything noticing.** The slot array
grows forward from the header while tuple data grows backward from the end. A
slot is a fixed-size (offset, length) pair, so a row is addressed by its slot
number, not by where its bytes sit — which is what lets compaction reclaim dead
space without updating a single outside reference.

**Two encodings, because they answer different questions.** A row encoding only
has to round-trip, so it is compact and self-describing. A *key* encoding has to
sort: the B+tree compares keys with `bytes.Compare` and knows nothing about
integers. So integers get their sign bit flipped, floats get the IEEE
transform, and text escapes its own terminator. Skip the sign flip and `-1`
becomes `0xFFFF...` and sorts above every positive number — the index is
silently, plausibly wrong.

**The volcano model, because memory should not scale with the table.** Every
operator is a thing you call `Next` on. A filter over a scan never materialises
the scan: it asks for one row, tests it, and passes it on. `LIMIT 10` over a
million rows reads eleven. The two operators that genuinely cannot stream —
sort and hash-aggregate — are exactly the ones that buffer, which is inherent
rather than a shortcut.

**Write-ahead logging, enforced in one place.** The rule is a sentence: a change
must be in the log, on disk, before the page holding it is written. The buffer
pool calls into the log before every page write, and that single hook is what
makes recovery possible — the log is guaranteed to know at least as much as the
data file does.

**Recovery is two passes, and it has to be.** The first learns which
transactions committed; the second replays only their page images. One pass
cannot work, because a transaction's changes appear in the log *before* its
commit record — at the moment a page image is read, whether to apply it is not
yet known. Full page images rather than deltas make replay idempotent, so it
does not matter how many times a record is applied.

**The catalog is stored in a B+tree like everything else.** Schema changes
travel through the same pages, buffer pool and log as ordinary data, so
`CREATE TABLE` is as crash-safe as `INSERT` without a second mechanism. SQLite
does the same thing, and exposes it as `sqlite_master`.

## Crash safety

Durability is only proven by killing the database, so the tests do:

```console
$ go test ./internal/db/ -run TestRecovery -v
    recovery: 103 records, 52 pages restored, 51 committed transactions
--- PASS: TestRecoveryReplaysCommittedWork
--- PASS: TestRecoveryDiscardsUncommittedWork
--- PASS: TestRecoveryDrawsTheLineAtCommit
    19 of 20 rows survived a torn log tail
--- PASS: TestRecoveryToleratesTornTail
--- PASS: TestRecoveryRejectsCorruptRecord
--- PASS: TestRecoveryIsIdempotent
```

Each of those abandons the process mid-flight — no checkpoint, no final flush,
every dirty page in the buffer pool discarded — and checks what the next open
reconstructs. A committed transaction survives. An uncommitted one leaves
nothing. A log torn mid-record still starts, keeping everything before the
tear. A record whose CRC fails is treated as the tear rather than applied.

## The wire protocol

Implementing someone else's protocol is the only honest test of a database's
interface. A bespoke client can be made to agree with whatever the server
happens to do; `psql` cannot, and it notices a wrong message length, a missing
`ReadyForQuery`, or a field count that disagrees with the row description.

The tests speak raw protocol bytes rather than going through a driver, because
that is the level the mistakes live at — including the ones that make a client
hang rather than complain:

- The startup handshake, and declining the SSL request so the client retries in
  plain text.
- `NULL` as length `-1`, which is a different value from an empty string.
- Booleans as `t` and `f`, which is what the text format specifies.
- Transaction status in every `ReadyForQuery`, which is what `psql` puts in its
  prompt — `=>` idle, `*>` in a transaction, `!>` failed.
- A `ReadyForQuery` after an error, without which the client waits forever.
- Real SQLSTATE codes: `42P01` for a missing table, `23505` for a duplicate key.

## Build and run

```console
$ go build -o bin/ ./cmd/...

$ quill shop.db                       # interactive shell
$ quill -c "SELECT * FROM users" shop.db
$ quill -serve :15433 shop.db         # psql can connect
```

| Flag | | |
|---|---|---|
| `-serve` | — | serve the PostgreSQL protocol on this address |
| `-c` | — | run one statement and exit |
| `-pool` | `1024` | buffer pool size in pages (4MB) |
| `-no-sync` | off | skip the fsync on commit: faster, loses recent commits on a crash |
| `-read-only` | off | refuse every statement that would write |

The shell has `\d` to list tables, `\d <table>` to describe one, `\stats` for
buffer pool activity, and `\checkpoint`.

## Testing

```console
$ go test -race ./...
ok  github.com/nafizahmed/quill/internal/db
ok  github.com/nafizahmed/quill/internal/storage
ok  github.com/nafizahmed/quill/internal/wire
```

The B+tree gets checked structurally rather than by results alone: `Verify()`
walks the tree asserting keys are sorted within every node, every key sits
inside the range its separators promise, and all leaves are at one depth. A
randomised 8,000-operation workload cross-checks the tree against a plain Go map
after every batch. Split logic is exactly the kind of code where an off-by-one
leaves a tree that still answers most queries correctly.

That caught the best bug in the project. `Init()` clears a page header, and the
leaf sibling pointer was being read *after* it rather than before — so every
split dropped the leaf chain. Point lookups still worked, because they descend
the tree. Only range scans broke, silently returning 309 rows where 1,026
existed.

Four more came from the SQL tests: the aggregate operator named its output
columns inconsistently with the planner's rewrite; `ORDER BY` could not name a
column the `SELECT` list dropped, which needed the sort moved *below* the
projection; grouped output lost its table qualifier so `GROUP BY u.name ORDER BY
u.name` failed to resolve; and truncating the log failed on Windows, which
refuses to shrink a file opened for append.

## Known limitations

Stated plainly, because an interviewer will ask.

- **One statement at a time.** A single mutex serialises everything, readers
  included. Real concurrency needs either page-level latching with a lock
  manager or MVCC with row versions, and each is a project in itself. The
  honest word is "serialised", not "concurrent".
- **Deletion does not rebalance.** Underfull B+tree nodes are left sparse rather
  than merged, so a table that is heavily deleted from wastes space and makes
  scans read more pages than they need.
- **The planner makes one decision.** If `WHERE` pins the primary key to a
  constant, it descends the tree; otherwise it scans. That is the decision worth
  the most, and it is the only one — there are no cardinality estimates and no
  cost comparison, so a secondary index is maintained on writes but never chosen
  for reads.
- **Joins are nested-loop only.** Correct for any condition, and O(n·m). A hash
  join would be far better for equi-joins on large inputs, and choosing between
  them is the planner work above.
- **Sort and aggregate are memory-bound.** Neither spills to disk, so a sort
  larger than RAM will fail rather than slow down.
- **A rollback invalidates the buffer cache**, because undo works by discarding
  dirty pages and re-reading. That avoids undo records entirely, and it means a
  very large transaction can exhaust the pool before reaching a commit.
- **No bound parameters.** The extended query protocol is implemented far enough
  for a driver to parse, bind and execute, but a statement containing `$1` is
  refused rather than silently mis-run.
- **No authentication.** Every connection is accepted. Doing it properly means
  SCRAM-SHA-256, which is real work rather than a stub.
- **Dropping a table leaks its pages** until a compaction pass exists.

## Roadmap

- [x] Slotted pages, buffer pool, B+tree, write-ahead log and recovery
- [x] SQL parser, planner, volcano executor, transactions
- [x] PostgreSQL wire protocol
- [ ] Secondary indexes chosen by the planner, not just maintained
- [ ] Hash joins and a cost-based choice between join strategies
- [ ] B+tree node merging on delete
- [ ] External merge sort, so ORDER BY can exceed memory
- [ ] MVCC, for readers that do not block writers
- [ ] Bound parameters and prepared statement reuse

## Layout

```
cmd/quill            binary: shell, one-shot, and server
internal/storage     pages, buffer pool, B+tree, write-ahead log
internal/types       value system, row and order-preserving key encodings
internal/catalog     schema, stored in its own B+tree
internal/sql         lexer, AST, recursive-descent parser
internal/exec        planner, expression evaluation, operators
internal/db          open, recover, sessions, transactions
internal/wire        PostgreSQL frontend/backend protocol
```
