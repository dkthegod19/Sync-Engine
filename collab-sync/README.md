# Syncpad — Real-Time Collaborative Sync Engine

The backend behind Google-Docs-style live editing, built from first principles in
Go. Multiple clients edit one document at once and converge to identical text;
the conflict resolution is a **CRDT (tree-based RGA) implemented from scratch** —
no CRDT library, and even the WebSocket layer is hand-rolled on the standard
library.

Read **[DESIGN.md](DESIGN.md)** for the architecture and the convergence proof.

## Run it

Requires Go 1.21+.

```bash
cd collab-sync
go run ./cmd/server         # http://localhost:8080
```

Open **two browser windows** at <http://localhost:8080> and type in both. You'll
see live cursors, presence avatars, and instant convergence. (Each window picks a
random name/colour; open an incognito window for a clearly separate identity.)

Custom address / web dir:

```bash
go run ./cmd/server -addr :9000 -web ./web
```

## Try the requirements

| # | Requirement | How to see it |
|---|---|---|
| 01 | Concurrent edits converge | Type in two windows at the same spot. |
| 02 | Conflict resolution from scratch | `internal/crdt/rga.go` — RGA, no library. |
| 03 | Offline edits merge | Click **Online → Offline**, keep typing in both windows, click **Offline → Online**. Queued edits merge, nothing is lost. |
| 04 | Full history + revert | Use the **Version history** sidebar: click a point to preview, **Restore** to revert. |
| 05 | Sub-second propagation | Status bar shows live round-trip ms. |
| 06 | Convergence proof | `go test ./...` (or `node tools/convergence.test.js`). |
| 07 | Disconnect/reconnect mid-edit | Stop the server while typing, restart it — the client auto-reconnects and re-syncs. |
| + | Presence (who's online, cursors) | Coloured remote carets + avatar row. |

## Tests

```bash
go test ./... -v                 # Go convergence/idempotency/offline/revert suite
node tools/convergence.test.js   # identical suite, no Go needed
```

The JS suite was run during development (verified: 25 seeds × 12 orderings
converge, plus idempotency, offline-merge, and revert). The Go suite is the same
algorithm and the same four tests.

## Layout

```
cmd/server/         entrypoint (HTTP + /ws + static files)
internal/crdt/      the RGA CRDT engine + tests  ← the core
internal/server/    stdlib WebSocket, message protocol, broadcast hub + history
web/                browser client (same RGA engine in JS) + UI
tools/              runnable JS convergence harness
```
