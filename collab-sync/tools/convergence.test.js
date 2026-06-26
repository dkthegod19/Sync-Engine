/*
 * convergence.test.js — empirical proof harness for the RGA CRDT.
 *
 * This mirrors internal/crdt/rga_test.go. It exercises the SAME algorithm that
 * the Go engine and the browser client use (web/crdt.js), so a pass here is
 * strong evidence the shared design converges. Run: node tools/convergence.test.js
 */
const { Doc, Site } = require("../web/crdt.js");

let failures = 0;
function check(name, cond) {
  if (cond) {
    console.log("  PASS  " + name);
  } else {
    console.log("  FAIL  " + name);
    failures++;
  }
}

// Small deterministic PRNG so failures are reproducible.
function mulberry32(seed) {
  return function () {
    seed |= 0;
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function shuffle(arr, rnd) {
  const a = arr.slice();
  for (let i = a.length - 1; i > 0; i--) {
    const j = Math.floor(rnd() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

// Generate a causally-valid op log by simulating several sites that each type
// and delete at random positions in their own replica, occasionally syncing.
function genOps(nSites, nOps, seed) {
  const rnd = mulberry32(seed);
  const sites = [];
  for (let i = 0; i < nSites; i++) sites.push(new Site("S" + i));
  const log = [];
  const alphabet = "abcdefghijklmnopqrstuvwxyz ".split("");

  for (let k = 0; k < nOps; k++) {
    const s = sites[Math.floor(rnd() * nSites)];
    const len = s.doc.visibleNodes().length;
    let op;
    if (len > 0 && rnd() < 0.25) {
      op = s.localDelete(Math.floor(rnd() * len));
    } else {
      const pos = Math.floor(rnd() * (len + 1));
      op = s.localInsert(pos, alphabet[Math.floor(rnd() * alphabet.length)]);
    }
    if (op) log.push(op);

    // periodically let a random other site catch up on everything so far,
    // which keeps later inserts referencing ids other sites may not have yet
    // (this is what creates interesting concurrent structure).
    if (rnd() < 0.5) {
      const other = sites[Math.floor(rnd() * nSites)];
      other.doc.applyMany(log);
    }
  }
  return log;
}

// ---- Test 1: order independence (the core convergence property) -----------
(function testOrderIndependence() {
  console.log("Test: same op set, many orders -> identical state");
  for (let seed = 1; seed <= 25; seed++) {
    const log = genOps(4, 200, seed);

    // canonical replica: apply in generation order
    const base = new Doc();
    base.applyMany(log);
    const baseText = base.text();

    // 12 random permutations must all converge to the same text
    let allEqual = true;
    for (let t = 0; t < 12; t++) {
      const perm = shuffle(log, mulberry32(seed * 1000 + t));
      const d = new Doc();
      d.applyMany(perm);
      if (d.text() !== baseText) {
        allEqual = false;
        console.log("    diverged on seed " + seed + " perm " + t);
        console.log("    base: " + JSON.stringify(baseText));
        console.log("    perm: " + JSON.stringify(d.text()));
        break;
      }
    }
    check("seed " + seed + " (" + baseText.length + " chars) converges across 12 orders", allEqual);
  }
})();

// ---- Test 2: idempotency / duplicate delivery (reconnect safety) ----------
(function testIdempotency() {
  console.log("Test: applying every op twice changes nothing");
  const log = genOps(3, 150, 7);
  const once = new Doc();
  once.applyMany(log);
  const twice = new Doc();
  twice.applyMany(log);
  twice.applyMany(log); // redeliver everything
  // also interleave duplicates
  for (const op of shuffle(log, mulberry32(99))) twice.apply(op);
  check("text identical after duplicate + redelivered ops", once.text() === twice.text());
})();

// ---- Test 3: offline merge (two partitions edit, then merge) --------------
(function testOfflineMerge() {
  console.log("Test: two clients edit offline, then exchange ops -> converge");
  let mism = 0;
  for (let seed = 1; seed <= 15; seed++) {
    const rnd = mulberry32(seed);
    const a = new Site("A");
    const b = new Site("B");

    // shared starting state
    const seed_ops = [];
    let s = new Site("BOOT");
    for (let i = 0; i < 10; i++) seed_ops.push(s.localInsert(i, "x"));
    a.doc.applyMany(seed_ops);
    b.doc.applyMany(seed_ops);
    a.counter = 100; // avoid id collisions for clarity (not required for correctness)
    b.counter = 200;

    // each goes offline and makes independent edits
    const aOps = [],
      bOps = [];
    for (let i = 0; i < 20; i++) {
      const al = a.doc.visibleNodes().length;
      aOps.push(rnd() < 0.3 && al > 0 ? a.localDelete(Math.floor(rnd() * al)) : a.localInsert(Math.floor(rnd() * (al + 1)), "A"));
      const bl = b.doc.visibleNodes().length;
      bOps.push(rnd() < 0.3 && bl > 0 ? b.localDelete(Math.floor(rnd() * bl)) : b.localInsert(Math.floor(rnd() * (bl + 1)), "B"));
    }
    // reconnect: exchange (in shuffled order, with one duplicate burst)
    a.doc.applyMany(shuffle(bOps.filter(Boolean), rnd));
    b.doc.applyMany(shuffle(aOps.filter(Boolean), rnd));
    b.doc.applyMany(bOps.filter(Boolean)); // duplicate redelivery
    if (a.doc.text() !== b.doc.text()) {
      mism++;
      console.log("    mismatch seed " + seed + ": " + JSON.stringify(a.doc.text()) + " vs " + JSON.stringify(b.doc.text()));
    }
  }
  check("15 offline-partition scenarios all converge", mism === 0);
})();

// ---- Test 4: revert by replay (history) -----------------------------------
(function testRevert() {
  console.log("Test: replaying a prefix of history reconstructs that version");
  const log = genOps(3, 120, 42);
  const full = new Doc();
  const snapshots = [];
  const fresh = new Doc();
  for (let i = 0; i < log.length; i++) {
    fresh.apply(log[i]);
    if (i === 40 || i === 80) snapshots.push({ at: i + 1, text: fresh.text() });
  }
  full.applyMany(log);

  let ok = true;
  for (const snap of snapshots) {
    const rebuilt = new Doc();
    rebuilt.applyMany(log.slice(0, snap.at));
    if (rebuilt.text() !== snap.text) ok = false;
  }
  check("prefix replay reproduces historical versions deterministically", ok);
})();

console.log("");
if (failures === 0) console.log("ALL TESTS PASSED");
else console.log(failures + " TEST(S) FAILED");
process.exit(failures === 0 ? 0 : 1);
