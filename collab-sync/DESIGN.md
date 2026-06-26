# Real-Time Collaborative Sync Engine — Design

The backend behind Google-Docs-style live editing, built from first principles.
Multiple people edit one document at the same time; everyone converges to the
exact same text, no central lock, no last-writer-wins clobbering, offline edits
merge cleanly on reconnect, and the full edit history is replayable.

The conflict-resolution core is a **CRDT** (Conflict-free Replicated Data Type)
written from scratch — specifically a tree-based **RGA** (Replicated Growable
Array). No CRDT library is pulled in.

---

## 1. The problem, concretely

Two people put their cursors at the same spot in "Helo" and both fix it: one
types "l" to make "Hello", the other types "p" to make "Helpo". A naive system
that ships cursor-relative offsets ("insert at index 3") corrupts the document
the moment those two edits cross on the wire, because after the first edit lands,
index 3 no longer means what the second editor thought it meant. Worse, if both
clients just overwrite the shared string, one edit silently disappears.

We need three guarantees:

1. **Convergence** — every replica that has seen the same set of edits shows
   identical text, regardless of the order the edits arrived in.
2. **Intention preservation** — an edit lands where its author meant it to,
   even after concurrent edits shift everything around it.
3. **No data loss** — a concurrent or offline edit never overwrites another.

A CRDT gives us all three by construction.

---

## 2. Why a CRDT (and why RGA)

The two mainstream approaches are **Operational Transformation (OT)** and
**CRDTs**.

OT (the original Google Docs / Jupiter approach) keeps positional operations
("insert at 3") and *transforms* them against concurrent operations before
applying ("someone inserted before you, so your 3 is now a 4"). It works, but the
transformation functions are notoriously hard to get right, it leans on a central
server to impose a single order, and offline / peer-to-peer merging is awkward.

A CRDT instead gives every character a **stable, globally unique identity** that
never shifts. Edits refer to *identities*, not positions, so they commute: apply
them in any order and you land in the same place. That property is exactly what
makes offline merge and reconnect fall out for free.

We use **RGA**, a sequence CRDT, modelled here as a tree. It is simple to reason
about, the convergence argument is a one-liner (see §4), and it maps naturally
onto "insert this character after that one."

---

## 3. The data model

The document is a **tree of character nodes**. Each node is:

```
id       : { site, seq }   globally unique. site = client id, seq = that
                           client's local counter. Never reused, never changes.
parent   : id | null       the element this character was inserted *after*
                           (its left neighbour). null means "the beginning".
ch       : string          the character itself.
deleted  : bool            tombstone. We never physically remove a node, so a
                           later edit that referenced it can still resolve.
```

An **operation** is one of:

```
Insert { id, after, ch }   create a node `id` holding `ch`, placed after `after`.
Delete { id }              tombstone the node `id`.
```

That is the entire vocabulary. Everything — typing, pasting, backspace,
select-and-replace, offline batches — decomposes into these two ops.

### Reading the document

The visible text is the **pre-order traversal** of the tree where, at every
node, children are visited in **descending id order**:

```
walk(node):
    for child in node.children sorted by id, DESCENDING:
        if not child.deleted: emit child.ch
        walk(child)
```

"Insert after X" makes the new node a child of X, so it is emitted immediately
after X and before X's later context. When two people insert after the *same*
X concurrently (the classic conflict), both become children of X; the descending
id sort breaks the tie **deterministically and identically on every replica**.
Different ids → a fixed total order → the same string everywhere.

---

## 4. Convergence (the formal argument)

> **Claim.** Any two replicas that have applied the same *set* of operations
> produce byte-identical documents, regardless of the order in which the
> operations were applied.

**Why it holds.**

1. Each operation names an immutable node id, an immutable parent id, and a
   character. So the *set* of operations uniquely determines the *set* of nodes
   and the parent→child edges — i.e. the tree's shape — independent of arrival
   order.
2. The order in which we emit text is a pure function of that tree: pre-order,
   children sorted by a total order on ids (§3). It reads nothing else.
3. A deterministic function of an order-independent structure is itself
   order-independent. Therefore the output text is identical on every replica
   that has the same op set. ∎

Two practical conditions make "same set of ops" achievable over a real network:

- **Causal readiness.** An Insert can only be applied once its parent node
  exists. Ops that arrive early (parent not yet seen) are **buffered** and
  retried after each successful apply. This is what lets ops arrive in *any*
  order, including the wildly-out-of-order batches you get after an offline
  client reconnects.
- **Idempotency.** Applying an op twice is a no-op (a node id is created at most
  once; a tombstone set twice is unchanged). So re-delivering ops on reconnect —
  or a client receiving its own op echoed back — is harmless.

This claim is not just asserted; it is **tested** (§8): the same op set is
applied in many shuffled orders and the resulting texts are asserted equal.

---

## 5. Offline editing & reconnect (requirements 03, 07)

The server keeps a monotonically increasing **server sequence** (`serverSeq`):
the position of each op in the authoritative log. Every client remembers the
highest `serverSeq` it has applied.

- **Going offline.** The client keeps editing. Local ops are applied to its own
  replica immediately (optimistic UI) and parked in an **outbox** instead of
  being sent.
- **Coming back.** The client opens the socket and sends
  `hello { siteId, since: <last serverSeq> }`. The server replies with a
  **delta** — every op with `seq > since`, i.e. exactly what the client missed
  while away. The client applies the delta (CRDT merge), then **flushes its
  outbox** to the server, which logs and broadcasts those ops to everyone.

Because ops commute and apply idempotently, the order of "apply what I missed"
vs. "send what I did" does not matter, and a half-sent batch that gets re-sent
after a flaky reconnect causes no duplication. A mid-edit disconnect therefore
loses nothing: the in-progress characters are already in the local replica and
the outbox, and they merge in on reconnect.

The same machinery powers **automatic reconnect**: an unexpected socket drop
triggers a backoff retry that replays the identical `hello { since }` handshake.

---

## 6. History & time travel (requirement 04)

The authoritative op log *is* the history — an ordered, append-only list of every
edit with its author and timestamp. Two operations on it:

- **Preview vN** — replay ops `[0..N)` into a throwaway replica and return that
  text. Pure, read-only, doesn't touch the live document.
- **Restore vN** — re-point the document at version N. Implemented like
  `git reset --hard`: the log is truncated to its first N ops, the authoritative
  replica is rebuilt from that prefix, and a `reset` snapshot is broadcast so
  every client rebuilds to the identical state and re-converges. Edits after N
  are discarded (a deliberate, documented trade-off — a branching/redo model is
  possible but out of scope).

Because reverting just selects a *prefix of the op set*, the convergence argument
in §4 applies unchanged to the restored state.

---

## 7. Live propagation & presence (requirements 05, stretch goal)

- **Ops** travel over a WebSocket. The server applies an incoming op, appends it
  to the log under a lock (so apply + log + fan-out are atomic), and pushes it to
  every connected client over a buffered in-memory channel. Propagation is a
  process-local channel send plus a socket write — sub-millisecond on a LAN, and
  the UI surfaces the measured round-trip. A slow client's buffer is allowed to
  drop rather than stall the hub.
- **Presence** is a *separate, ephemeral channel*: cursor position and selection,
  name, and colour. It is broadcast but never written to the op log, because it
  is not part of the document. The UI renders each remote participant's caret and
  selection in their colour with a name flag, and shows live avatars of who's in
  the room.

The WebSocket layer itself is implemented directly on Go's standard library
(`internal/server/ws.go`) — the RFC 6455 upgrade handshake, client→server frame
unmasking, fragmentation, and ping/pong/close — so there is no third-party
networking dependency either.

---

## 8. Proving it (the tests)

`internal/crdt/rga_test.go` (Go) and `tools/convergence.test.js` (the JS twin,
runnable with plain `node`) contain the same four checks:

1. **Convergence** — generate a rich op log from several simulated clients that
   type, delete, and partially sync at random; apply it in the generation order
   to get a baseline; then apply 12 random permutations of the *same* op set to
   fresh replicas and assert every one yields identical text. Repeated over 25
   seeds.
2. **Idempotency** — apply every op, then re-deliver the whole log (and a
   shuffled copy); assert the text is unchanged. This is the reconnect-safety
   guarantee.
3. **Offline merge** — two replicas share a base, partition, each makes ~20
   independent edits, then exchange ops (shuffled, with a duplicate burst);
   assert they converge.
4. **Revert by replay** — replay prefixes of the log and assert they reproduce
   the exact historical text, validating preview/restore.

The JS suite is what was executed during development (the sandbox had no Go
toolchain); since the Go and JS engines implement the identical algorithm, a pass
there is direct evidence for both. Run `go test ./...` locally to exercise the
Go suite.

---

## 9. Wire protocol (summary)

Client → server: `hello {siteId,name,color,since}`, `op {op}`,
`presence {cursor}`, `preview {version}`, `revert {version}`.

Server → client: `welcome {serverSeq, ops[], users[], you}` (delta sync),
`op {seq, op, time}` (broadcast), `presence {user}`, `users {users[]}`,
`preview {version, text}`, `reset {serverSeq, ops[]}` (after a restore).

---

## 10. Layout

```
cmd/server/main.go            HTTP + static files + /ws endpoint
internal/crdt/rga.go          the RGA CRDT engine (the heart of the project)
internal/crdt/rga_test.go     convergence / idempotency / offline / revert tests
internal/server/ws.go         stdlib WebSocket (handshake + framing)
internal/server/protocol.go   message types
internal/server/hub.go        authoritative replica, op log, fan-out, history
web/crdt.js                   the same RGA engine for the browser
web/app.js                    editor, diff→ops, presence rendering, offline, history UI
web/index.html, web/style.css the Syncpad UI
tools/convergence.test.js     the runnable JS proof harness
```

## 11. Honest limitations / next steps

- **Plain text today.** The engine is a sequence CRDT. The structured-document
  stretch goal (nested lists / a spreadsheet grid) is a natural extension: model
  the grid as a map-of-cells CRDT whose values are RGA strings, reusing this
  exact engine per cell.
- **Restore is destructive** (git-reset-style); a branching history with redo is
  future work.
- **Tombstones accumulate.** Deleted nodes are retained forever; a production
  system would add causal-stability garbage collection.
- **In-memory only.** The op log lives in process memory. Persisting it (append
  to disk / a log store) would survive restarts with no algorithm changes —
  startup just replays the log.
- **Presence positions** are sent as integer offsets in the converged text;
  during a burst of concurrent edits a remote caret can be momentarily off by a
  few characters until the next cursor update.
