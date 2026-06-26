/* app.js — browser client for the collaborative sync engine.
 *
 * Responsibilities:
 *   - hold a local CRDT replica (web/crdt.js) and apply edits optimistically
 *   - translate textarea changes into insert/delete ops via a prefix/suffix diff
 *   - send/receive ops over a WebSocket, converging with every other client
 *   - render remote presence (live coloured cursors + selections)
 *   - keep working while "offline" (queue ops) and re-sync on reconnect
 *   - drive the version-history timeline (preview + restore)
 */
(function () {
  "use strict";

  // ---- identity (persisted so reconnect keeps the same site id) -----------
  var NAMES = ["Ava", "Ben", "Cleo", "Devi", "Eli", "Mara", "Nia", "Omar", "Pia", "Rune", "Sana", "Theo"];
  var COLORS = ["#2563eb", "#dc2626", "#16a34a", "#d97706", "#7c3aed", "#db2777", "#0891b2", "#4f46e5"];
  function persist(k, gen) {
    var v = localStorage.getItem(k);
    if (!v) { v = gen(); localStorage.setItem(k, v); }
    return v;
  }
  var me = {
    id: persist("sp_site", function () { return "u-" + Math.random().toString(36).slice(2, 8); }),
    name: persist("sp_name", function () { return NAMES[(Math.random() * NAMES.length) | 0]; }),
    color: persist("sp_color", function () { return COLORS[(Math.random() * COLORS.length) | 0]; }),
  };

  // ---- state --------------------------------------------------------------
  var site = new CRDT.Site(me.id);
  var ws = null;
  var manualOffline = false;
  var serverSeq = 0;
  var opsApplied = 0;
  var outbox = [];                 // ops made while offline
  var users = new Map();           // siteId -> {siteId,name,color,cursor}
  var history = [];                // {seq, site, kind, time}
  var pingSentAt = new Map();      // opKey -> perf time (round-trip measure)
  var lastValue = "";
  var previewTarget = 0;

  // ---- dom ----------------------------------------------------------------
  var editor = document.getElementById("editor");
  var mirror = document.getElementById("mirror");
  var stage = document.getElementById("editorStage");
  var cursorLayer = document.getElementById("cursorLayer");
  var avatarsEl = document.getElementById("avatars");
  var netBtn = document.getElementById("netBtn");
  var netLabel = document.getElementById("netLabel");
  var timelineEl = document.getElementById("timeline");
  var previewPanel = document.getElementById("previewPanel");
  var previewText = document.getElementById("previewText");
  var previewVer = document.getElementById("previewVer");

  function opKey(op) { return op.kind + ":" + op.id.site + "#" + op.id.seq; }

  // ---- diff: textarea value change -> CRDT ops ----------------------------
  function diffToOps(oldS, newS) {
    if (oldS === newS) return [];
    var p = 0, min = Math.min(oldS.length, newS.length);
    while (p < min && oldS.charCodeAt(p) === newS.charCodeAt(p)) p++;
    var s = 0;
    while (s < min - p && oldS.charCodeAt(oldS.length - 1 - s) === newS.charCodeAt(newS.length - 1 - s)) s++;
    var delCount = oldS.length - p - s;
    var ins = newS.slice(p, newS.length - s);
    var ops = [];
    for (var i = 0; i < delCount; i++) {
      var d = site.localDelete(p);
      if (d) ops.push(d);
    }
    for (var j = 0; j < ins.length; j++) {
      ops.push(site.localInsert(p + j, ins[j]));
    }
    return ops;
  }

  function onLocalInput() {
    var next = editor.value;
    var ops = diffToOps(lastValue, next);
    lastValue = next;
    for (var i = 0; i < ops.length; i++) sendOp(ops[i]);
    autogrow();
    updateStats();
    scheduleCursor();
  }

  function sendOp(op) {
    if (isOnline()) {
      ws.send(JSON.stringify({ type: "op", op: op }));
      pingSentAt.set(opKey(op), performance.now());
    } else {
      outbox.push(op);
      updateStats();
    }
  }

  // ---- applying server-confirmed ops --------------------------------------
  function applyServerOp(so, live) {
    var op = so.op;
    var mine = op.id.site === me.id;
    site.doc.apply(op); // idempotent: our own optimistic op is a no-op here
    opsApplied++;
    if (so.seq && so.seq > serverSeq) serverSeq = so.seq;
    history.push({ seq: so.seq, site: op.id.site, kind: op.kind, time: so.time });
    if (live && mine) {
      var k = opKey(op);
      if (pingSentAt.has(k)) { setPing(performance.now() - pingSentAt.get(k)); pingSentAt.delete(k); }
    }
    return mine;
  }

  // ---- websocket ----------------------------------------------------------
  function wsURL() {
    return (location.protocol === "https:" ? "wss" : "ws") + "://" + location.host + "/ws";
  }
  function isOnline() { return ws && ws.readyState === WebSocket.OPEN; }

  function connect() {
    try { ws = new WebSocket(wsURL()); } catch (e) { scheduleReconnect(); return; }
    ws.onopen = function () {
      setNet(true);
      ws.send(JSON.stringify({ type: "hello", siteId: me.id, name: me.name, color: me.color, since: serverSeq }));
      sendCursorNow();
    };
    ws.onmessage = function (ev) { onMessage(JSON.parse(ev.data)); };
    ws.onclose = function () { ws = null; setNet(false); if (!manualOffline) scheduleReconnect(); };
    ws.onerror = function () { try { ws.close(); } catch (e) {} };
  }
  function scheduleReconnect() {
    if (manualOffline) return;
    setTimeout(function () { if (!isOnline() && !manualOffline) connect(); }, 800);
  }

  function onMessage(m) {
    switch (m.type) {
      case "welcome": onWelcome(m); break;
      case "op": onRemoteOp(m); break;
      case "presence": onPresence(m); break;
      case "users": onUsers(m); break;
      case "preview": onPreviewResult(m); break;
      case "reset": onReset(m); break;
    }
  }

  function onWelcome(m) {
    var ops = m.ops || [];
    for (var i = 0; i < ops.length; i++) applyServerOp(ops[i], false);
    if (m.serverSeq) serverSeq = m.serverSeq;
    if (m.users) setUsers(m.users);
    flushOutbox();
    render();
    rebuildTimeline();
  }

  function onRemoteOp(m) {
    var mine = applyServerOp({ seq: m.seq, op: m.op, time: m.time }, true);
    if (!mine) render();      // someone else changed the text
    else { updateStats(); }   // our echo: text already shown, just refresh stats
    rebuildTimeline();
    scheduleCursor();
  }

  function onReset(m) {
    var keepCounter = site.counter; // preserve so new ids never collide with old ones
    site = new CRDT.Site(me.id);
    site.counter = keepCounter;
    history.length = 0;
    opsApplied = 0;
    var ops = m.ops || [];
    for (var i = 0; i < ops.length; i++) {
      site.doc.apply(ops[i].op);
      opsApplied++;
      history.push({ seq: ops[i].seq, site: ops[i].op.id.site, kind: ops[i].op.kind, time: ops[i].time });
    }
    serverSeq = m.serverSeq || 0;
    render();
    rebuildTimeline();
    hidePreview();
    toast("Document restored to <b>v" + serverSeq + "</b>");
  }

  // ---- presence -----------------------------------------------------------
  var cursorTimer = null;
  function scheduleCursor() {
    if (cursorTimer) return;
    cursorTimer = setTimeout(function () { cursorTimer = null; sendCursorNow(); }, 60);
  }
  function sendCursorNow() {
    if (!isOnline()) return;
    ws.send(JSON.stringify({ type: "presence", cursor: { pos: editor.selectionEnd, anchor: editor.selectionStart } }));
  }
  function onPresence(m) {
    if (!m.user) return;
    var ex = users.get(m.user.siteId) || {};
    var merged = Object.assign({}, ex, m.user);
    users.set(m.user.siteId, merged);
    drawCursors();
  }
  function onUsers(m) { setUsers(m.users || []); }
  function setUsers(arr) {
    var prev = users;
    users = new Map();
    for (var i = 0; i < arr.length; i++) {
      var u = arr[i];
      var old = prev.get(u.siteId);
      if (old && old.cursor && !u.cursor) u.cursor = old.cursor;
      users.set(u.siteId, u);
    }
    renderAvatars();
    drawCursors();
  }

  function renderAvatars() {
    avatarsEl.innerHTML = "";
    var list = Array.from(users.values());
    list.sort(function (a, b) { return a.siteId === me.id ? -1 : b.siteId === me.id ? 1 : 0; });
    list.forEach(function (u) {
      var a = document.createElement("div");
      a.className = "avatar" + (u.siteId === me.id ? " you" : "");
      a.style.background = u.color || "#888";
      a.textContent = (u.name || "?").slice(0, 1).toUpperCase();
      a.title = u.name + (u.siteId === me.id ? " (you)" : "");
      avatarsEl.appendChild(a);
    });
  }

  // ---- remote cursor rendering (mirror measurement) -----------------------
  function caretCoords(pos) {
    pos = Math.max(0, Math.min(pos, editor.value.length));
    mirror.textContent = editor.value.slice(0, pos);
    var marker = document.createElement("span");
    marker.textContent = "\u200b";
    mirror.appendChild(marker);
    var sr = stage.getBoundingClientRect();
    var mr = marker.getBoundingClientRect();
    var lh = parseFloat(getComputedStyle(editor).lineHeight) || 28;
    var c = { x: mr.left - sr.left, y: mr.top - sr.top, h: lh };
    mirror.textContent = "";
    return c;
  }

  function drawCursors() {
    cursorLayer.innerHTML = "";
    users.forEach(function (u, sid) {
      if (sid === me.id || !u.cursor) return;
      if (u.cursor.anchor != null && u.cursor.anchor !== u.cursor.pos) {
        drawSelection(Math.min(u.cursor.anchor, u.cursor.pos), Math.max(u.cursor.anchor, u.cursor.pos), u.color);
      }
      var c = caretCoords(u.cursor.pos);
      var caret = document.createElement("div");
      caret.className = "remote-caret show-flag";
      caret.style.left = c.x + "px";
      caret.style.top = c.y + "px";
      caret.style.height = c.h + "px";
      caret.style.background = u.color;
      var flag = document.createElement("div");
      flag.className = "flag";
      flag.textContent = u.name;
      flag.style.background = u.color;
      caret.appendChild(flag);
      cursorLayer.appendChild(caret);
    });
  }

  function drawSelection(a, b, color) {
    var ca = caretCoords(a), cb = caretCoords(b);
    var w = stage.clientWidth;
    function rect(x, y, width, h) {
      var d = document.createElement("div");
      d.className = "remote-selection";
      d.style.background = color;
      d.style.left = x + "px"; d.style.top = y + "px";
      d.style.width = Math.max(2, width) + "px"; d.style.height = h + "px";
      cursorLayer.appendChild(d);
    }
    if (Math.abs(ca.y - cb.y) < 1) {
      rect(ca.x, ca.y, cb.x - ca.x, ca.h);
    } else {
      rect(ca.x, ca.y, w - ca.x, ca.h);
      if (cb.y - ca.y > ca.h + 1) rect(0, ca.y + ca.h, w, cb.y - ca.y - ca.h);
      rect(0, cb.y, cb.x, cb.h);
    }
  }

  // ---- render -------------------------------------------------------------
  function posAfterId(id) {
    if (!id) return 0;
    var v = site.doc.visibleNodes();
    for (var i = 0; i < v.length; i++) if (v[i].id.site === id.site && v[i].id.seq === id.seq) return i + 1;
    return 0;
  }

  function render() {
    var text = site.doc.text();
    var focused = document.activeElement === editor;
    var leftId = focused ? site.doc.idAt(editor.selectionStart) : null;
    if (editor.value !== text) editor.value = text;
    lastValue = text;
    if (focused) {
      var pos = posAfterId(leftId);
      editor.selectionStart = editor.selectionEnd = pos;
    }
    autogrow();
    updateStats();
    drawCursors();
  }

  function autogrow() {
    editor.style.height = "auto";
    editor.style.height = editor.scrollHeight + "px";
  }

  // ---- stats --------------------------------------------------------------
  function updateStats() {
    document.getElementById("statChars").textContent = site.doc.text().length;
    document.getElementById("statOps").textContent = opsApplied;
    document.getElementById("statSeq").textContent = serverSeq;
    document.getElementById("statPending").textContent = outbox.length;
    document.getElementById("statPendingWrap").hidden = outbox.length === 0;
  }
  function setPing(ms) {
    document.getElementById("statPing").textContent = Math.round(ms) + " ms round-trip";
  }

  // ---- offline / network toggle -------------------------------------------
  netBtn.onclick = function () {
    if (manualOffline) {
      manualOffline = false;
      connect();
      toast("Reconnecting — your <b>" + outbox.length + "</b> offline edit(s) will merge in");
    } else {
      manualOffline = true;
      if (ws) try { ws.close(); } catch (e) {}
      setNet(false);
      toast("You are offline. Keep typing — edits are queued and will sync on reconnect.");
    }
  };
  function flushOutbox() {
    if (!isOnline() || outbox.length === 0) return;
    var pending = outbox; outbox = [];
    for (var i = 0; i < pending.length; i++) {
      ws.send(JSON.stringify({ type: "op", op: pending[i] }));
      pingSentAt.set(opKey(pending[i]), performance.now());
    }
    updateStats();
  }
  function setNet(online) {
    netBtn.className = "btn net " + (online ? "online" : "offline");
    netLabel.textContent = online ? "Online" : (manualOffline ? "Offline" : "Reconnecting…");
  }

  // ---- history timeline ---------------------------------------------------
  function userMeta(siteId) {
    var u = users.get(siteId);
    if (u) return { name: u.name, color: u.color };
    // fallback for authors who already left
    var h = 0; for (var i = 0; i < siteId.length; i++) h = (h * 31 + siteId.charCodeAt(i)) >>> 0;
    return { name: siteId, color: COLORS[h % COLORS.length] };
  }

  function groupedHistory() {
    var groups = [];
    for (var i = 0; i < history.length; i++) {
      var h = history[i];
      var last = groups[groups.length - 1];
      if (last && last.site === h.site && h.time - last.time < 4000) {
        last.seq = h.seq; last.time = h.time; last.count++;
        if (h.kind === "ins") last.ins++; else last.del++;
      } else {
        groups.push({ site: h.site, seq: h.seq, time: h.time, count: 1, ins: h.kind === "ins" ? 1 : 0, del: h.kind === "del" ? 1 : 0 });
      }
    }
    return groups;
  }

  function fmtTime(ms) {
    var d = new Date(ms);
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function rebuildTimeline() {
    timelineEl.innerHTML = "";
    var groups = groupedHistory();
    if (groups.length === 0) {
      var e = document.createElement("li");
      e.className = "empty";
      e.textContent = "No edits yet. Start typing — every change is versioned here.";
      timelineEl.appendChild(e);
      return;
    }
    groups.reverse(); // newest first
    groups.forEach(function (g, idx) {
      var meta = userMeta(g.site);
      var li = document.createElement("li");
      li.dataset.seq = g.seq;
      if (idx === 0) li.classList.add("active");
      var dot = document.createElement("span");
      dot.className = "tl-dot"; dot.style.background = meta.color;
      var body = document.createElement("div");
      body.className = "tl-body";
      var line = document.createElement("div");
      line.className = "tl-line";
      line.innerHTML = "<b>" + esc(meta.name) + (g.site === me.id ? " (you)" : "") + "</b> edited";
      var sub = document.createElement("div");
      sub.className = "tl-meta";
      var parts = [];
      if (g.ins) parts.push("+" + g.ins);
      if (g.del) parts.push("−" + g.del);
      sub.textContent = "v" + g.seq + " · " + parts.join(" ") + " · " + fmtTime(g.time);
      body.appendChild(line); body.appendChild(sub);
      li.appendChild(dot); li.appendChild(body);
      li.onclick = function () { preview(g.seq); };
      timelineEl.appendChild(li);
    });
  }

  function preview(version) {
    previewTarget = version;
    if (isOnline()) ws.send(JSON.stringify({ type: "preview", version: version }));
    Array.prototype.forEach.call(timelineEl.children, function (li) {
      li.classList.toggle("active", li.dataset.seq == version);
    });
  }
  function onPreviewResult(m) {
    previewPanel.hidden = false;
    previewVer.textContent = "v" + m.version;
    previewText.textContent = m.text || "(empty document)";
  }
  function hidePreview() { previewPanel.hidden = true; }

  document.getElementById("restoreBtn").onclick = function () {
    if (isOnline()) ws.send(JSON.stringify({ type: "revert", version: previewTarget }));
    else toast("Reconnect to restore a version.");
  };
  document.getElementById("closePreview").onclick = function () {
    hidePreview();
    rebuildTimeline();
  };

  // ---- misc ---------------------------------------------------------------
  function esc(s) { return String(s).replace(/[&<>]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]; }); }
  var toastTimer = null;
  function toast(html) {
    var t = document.getElementById("toast");
    t.innerHTML = html; t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.hidden = true; }, 3200);
  }

  // ---- wire up ------------------------------------------------------------
  editor.addEventListener("input", onLocalInput);
  editor.addEventListener("keyup", scheduleCursor);
  editor.addEventListener("click", scheduleCursor);
  document.addEventListener("selectionchange", function () {
    if (document.activeElement === editor) scheduleCursor();
  });
  window.addEventListener("resize", drawCursors);

  autogrow();
  updateStats();
  rebuildTimeline();
  connect();
})();
