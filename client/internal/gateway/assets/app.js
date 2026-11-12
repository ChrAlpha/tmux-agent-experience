// tmag-client: the agents in tmux on each machine, from a phone.
//
// Everything shown comes from the gateway's snapshot of each machine, then
// the events it relays: an event with a state reports its session in full,
// and the latest pane list follows. Every action is one tmag verb, which
// checks its own gate on the machine.
"use strict";

const ENDED = new Set(["SessionEnd", "Exit"]);
const REPORTED = ["state", "kind", "turn", "seen", "cwd", "runtime", "transcript", "subagents"];
const SECTION_ORDER = { inbox: 0, working: 1, settled: 2, unknown: 3 };
const STALE = 45000; // a stream silent this long is presumed dead
const AUTHORIZATION = "Authorization";
const CREDENTIAL_KEY = "tmag-credential";
const IOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
const STANDALONE = window.matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;

// The credential admits this device. It lives in this origin's storage, never
// in a cookie, and goes with each request as a header: no other page, and no
// other port on the machine, can make the browser send it.
let credential = "";

// takeInvitation reads the code serve or auth pair printed, and keeps it out
// of the address and history.
function takeInvitation() {
  const m = location.hash.match(/^#pair=([^&]+)$/);
  if (!m) return "";
  history.replaceState(null, "", location.pathname + location.search);
  return decodeURIComponent(m[1]);
}

// A route is the inbox, one session, or one request; anything else opens the
// inbox.
const ROUTE = /^#\/(?:s\/[^/?#]+(?:\/r\/[^/?#]+)?)?$/;
const validRoute = route => typeof route === "string" && ROUTE.test(route);
const decodePart = value => { try { return decodeURIComponent(value); } catch { return value; } };

// openRoute shows a route a notification names. WebKit does not reliably
// carry out WindowClient.navigate, so the service worker posts the route here.
function openRoute(route) {
  if (validRoute(route) && location.hash !== route) location.hash = route;
}

// takeOpenRoute moves the open parameter an opened window carries into the
// hash, and removes it from the address and history.
function takeOpenRoute() {
  const params = new URLSearchParams(location.search);
  if (!params.has("open")) return "";
  const route = params.get("open");
  params.delete("open");
  const search = params.toString();
  const hash = validRoute(route) ? route : location.hash;
  history.replaceState(null, "", location.pathname + (search ? "?" + search : "") + hash);
  return validRoute(route) ? route : "";
}

const ROUTE_DB = "tmag-route";
const ROUTE_STORE = "route";
const ROUTE_KEY = "tapped";
const ROUTE_FRESH = 2 * 60 * 1000;
const ALERTS_STORE = "alerts";
const ALERT_TTL = 10 * 60 * 1000;
const DB_VERSION = 2;

function routeDB() {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(ROUTE_DB, DB_VERSION);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(ROUTE_STORE)) db.createObjectStore(ROUTE_STORE);
      if (!db.objectStoreNames.contains(ALERTS_STORE)) db.createObjectStore(ALERTS_STORE, { keyPath: "tag" });
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

// takeTappedRoute applies the route a notification tap left in IndexedDB. The
// page may have reloaded, or the worker may have found no window to post to.
function takeTappedRoute() {
  if (!("indexedDB" in window)) return Promise.resolve("");
  return routeDB().then(db => new Promise((resolve, reject) => {
    let record = null;
    const tx = db.transaction(ROUTE_STORE, "readwrite");
    const store = tx.objectStore(ROUTE_STORE);
    const get = store.get(ROUTE_KEY);
    get.onsuccess = () => {
      record = get.result;
      if (record) store.delete(ROUTE_KEY);
    };
    tx.oncomplete = () => {
      db.close();
      if (record && validRoute(record.route) && typeof record.at === "number" && Date.now() - record.at < ROUTE_FRESH) {
        openRoute(record.route);
        resolve(record.route);
      } else {
        resolve("");
      }
    };
    tx.onerror = () => { db.close(); reject(tx.error); };
    tx.onabort = () => { db.close(); reject(tx.error); };
  })).catch(() => "");
}

function readAlerts() {
  return routeDB().then(db => new Promise((resolve, reject) => {
    const tx = db.transaction(ALERTS_STORE, "readonly");
    const all = tx.objectStore(ALERTS_STORE).getAll();
    let alerts = [];
    all.onsuccess = () => { alerts = all.result || []; };
    tx.oncomplete = () => { db.close(); resolve(alerts); };
    tx.onerror = () => { db.close(); reject(tx.error); };
    tx.onabort = () => { db.close(); reject(tx.error); };
  }));
}

function forgetAlerts(tags) {
  if (!tags.length) return Promise.resolve();
  return routeDB().then(db => new Promise((resolve, reject) => {
    const tx = db.transaction(ALERTS_STORE, "readwrite");
    const store = tx.objectStore(ALERTS_STORE);
    for (const tag of tags) store.delete(tag);
    tx.oncomplete = () => { db.close(); resolve(); };
    tx.onerror = () => { db.close(); reject(tx.error); };
    tx.onabort = () => { db.close(); reject(tx.error); };
  }));
}

// takeAlertRoute opens the newest recent alert no longer shown, which the
// person took. It always forgets taken and expired alerts; open says whether
// a tap's own route did not already win.
async function takeAlertRoute(open) {
  if (!("indexedDB" in window)) return "";
  let alerts;
  try { alerts = await readAlerts(); } catch { return ""; }
  const now = Date.now();
  const recent = [];
  const expired = [];
  for (const alert of alerts) {
    if (!alert || typeof alert.tag !== "string") continue;
    if (typeof alert.at !== "number" || now - alert.at >= ALERT_TTL) expired.push(alert.tag);
    else recent.push(alert);
  }
  try { await forgetAlerts(expired); } catch { /* taking the route still goes on */ }
  if (!recent.length) return "";
  if (!navigator.serviceWorker || typeof navigator.serviceWorker.getRegistration !== "function") return "";
  let shown;
  try {
    const registration = await navigator.serviceWorker.getRegistration();
    if (!registration || typeof registration.getNotifications !== "function") return "";
    shown = await registration.getNotifications();
  } catch {
    return "";
  }
  const displayed = new Set((shown || []).map(notification => notification.tag || ""));
  const taken = recent.filter(alert => !displayed.has(alert.tag));
  try { await forgetAlerts(taken.map(alert => alert.tag)); } catch { /* taking the route still goes on */ }
  if (!open) return "";
  const valid = taken.filter(alert => validRoute(alert.route)).sort((a, b) => b.at - a.at);
  if (!valid.length) return "";
  openRoute(valid[0].route);
  return valid[0].route;
}

if ("serviceWorker" in navigator) {
  navigator.serviceWorker.addEventListener("message", event => {
    if (event.data && event.data.type === "tmag-route") openRoute(event.data.route);
  });
  navigator.serviceWorker.startMessages();
}

function setCredential(token) {
  credential = token;
  localStorage.setItem(CREDENTIAL_KEY, token);
}

// ---- state ----

const st = {
  machines: [], // names, this machine first
  version: "",
  m: new Map(), // name -> { seq, sessions: Map(agent/id -> session), panes, up, error }
  connected: false,
  details: new Map(), // machine/request -> { state, doc, error }
  answered: new Set(), // machine/request answered from here, until it closes
  choices: new Map(), // machine/request -> { question: label }
  notes: new Map(), // machine/request -> deny note, shown or not
  drafts: new Map(), // machine/session -> prompt being written
  busy: new Set(), // actions in flight
  armed: "", // the interrupt waiting for its second tap
  replies: new Map(), // machine/session -> { turn, state, text, error }
  screens: new Map(), // machine/session -> { state, text, at, error }
  check: "", // the last comparison with tmag list
  refused: "", // why tmag-client refused the stream last, until it accepts it
  pairing: null, // { error, busy } while this device needs pairing
  seenAttempts: new Set(), // session turns this page already asked to mark seen
  seenErrors: new Map(), // machine/agent/id:turn -> why the mark failed
  push: { enabled: false, subscribed: false, key: "", reason: "" },
  pushBusy: false,
  credential: null, // { id, label }
};

function machine(name) {
  let m = st.m.get(name);
  if (!m) {
    m = { seq: null, sessions: new Map(), panes: [], up: false, error: "" };
    st.m.set(name, m);
  }
  return m;
}

const skey = (agent, id) => agent + "/" + id;
const tkey = (machine, agent, id) => machine + "/" + agent + "/" + id;

function apply(name, doc) {
  const m = machine(name);
  if (doc.error) {
    m.error = doc.error.message || doc.error.code;
    return;
  }
  switch (doc.type) {
    case "snapshot":
      m.seq = doc.seq;
      m.sessions = new Map((doc.sessions || []).map(s => [skey(s.agent, s.id), normal(s)]));
      m.panes = doc.panes || [];
      break;
    case "panes":
      m.panes = doc.panes || [];
      break;
    case "event":
      applyEvent(m, doc.event);
      break;
  }
}

function applyEvent(m, e) {
  if (m.seq !== null && typeof e.seq === "number" && e.seq <= m.seq) return;
  m.seq = e.seq;
  const k = skey(e.agent, e.session);
  if (ENDED.has(e.event)) {
    m.sessions.delete(k);
    return;
  }
  const prev = m.sessions.get(k);
  // A Seen event reports the turn the person has seen, and changes nothing
  // else about the session.
  if (e.event === "Seen") {
    if (prev && e.turn) {
      prev.seen = e.turn;
      m.sessions.set(k, prev);
    }
    return;
  }
  // Only an event with a state speaks for the session as a whole; a field
  // left out is empty.
  if (!e.state) return;
  const s = { id: e.session, agent: e.agent, capabilities: e.capabilities, requests: e.requests };
  for (const f of REPORTED) s[f] = e[f];
  const same = prev && prev.state === e.state && (prev.kind || "") === (e.kind || "");
  s.since = same ? prev.since : e.time;
  m.sessions.set(k, normal(s));
}

// normal gives a session its lists, which the contract leaves out when
// empty.
function normal(s) {
  s.capabilities = s.capabilities || [];
  s.requests = s.requests || [];
  s.subagents = s.subagents || [];
  return s;
}

// all is every session of every machine, with the panes that show it,
// ordered as the inbox reads: what waits for the person, what works, what is
// settled, then what does not report; longest waiting first inside a group.
function all() {
  const out = [];
  for (const [name, m] of st.m) {
    for (const s of m.sessions.values()) {
      out.push({ machine: name, s, panes: m.panes.filter(p => p.session === s.id && p.agent === s.agent) });
    }
  }
  return out.sort((a, b) => (SECTION_ORDER[sectionOf(a)] ?? 9) - (SECTION_ORDER[sectionOf(b)] ?? 9) || String(waiting(a)).localeCompare(String(waiting(b))));
}

function isSeen(x) {
  return !!x.s.turn && x.s.seen === x.s.turn;
}

function sectionOf(x) {
  const s = x.s;
  if (s.state === "blocked") return "inbox";
  if (s.state === "idle" && s.turn && !isSeen(x)) return "inbox";
  if (s.state === "working") return "working";
  if (s.state === "idle") return "settled";
  return "unknown";
}

function waiting(x) {
  const s = x.s;
  if (s.state === "blocked" && s.requests.length) {
    return s.requests.map(q => String(q.since)).sort()[0];
  }
  return s.since || "";
}

function find(id) {
  return all().find(x => x.s.id === id) || null;
}

// target names a session for a verb: the pane that shows the main agent when
// several do, otherwise the session itself.
function target(x) {
  const main = x.panes.filter(p => p.view !== "subagent");
  return main.length > 1 ? main[0].server + ":" + main[0].id : x.s.id;
}

// ---- the stream ----

// The stream is read with fetch, which can send the credential, rather than
// EventSource, which cannot; its server-sent events are parsed here.
let stream = null; // the AbortController of the stream being read
let retry = 0;
let heard = 0;

async function connect() {
  // One stream at a time, whoever asked for it.
  clearTimeout(retryTimer);
  if (stream) stream.abort();
  const ctl = new AbortController();
  stream = ctl;
  let res;
  try {
    res = await fetch("/api/watch",
      { headers: { [AUTHORIZATION]: "Bearer " + credential }, signal: ctl.signal, cache: "no-store" });
  } catch {
    if (stream === ctl) reconnect(false);
    return;
  }
  if (res.status === 401) {
    needPairing();
    return;
  }
  if (!res.ok || !res.body) {
    // Refused by a tunnel's check, which may pass in a moment: keep the
    // credential, say why, and try again.
    try { st.refused = (JSON.parse(await res.text()).error || {}).message || res.statusText; } catch { st.refused = res.statusText; }
    if (stream === ctl) reconnect(false);
    return;
  }
  retry = 0;
  heard = Date.now();
  st.connected = true;
  st.refused = "";
  render();
  const reader = res.body.getReader();
  const text = new TextDecoder();
  let buf = "";
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += text.decode(value, { stream: true });
      for (let i; (i = buf.indexOf("\n\n")) >= 0; buf = buf.slice(i + 2)) frame(buf.slice(0, i));
    }
  } catch { /* aborted, or the connection dropped */ }
  if (stream === ctl) reconnect(false);
}

// frame handles one server-sent event: a document of a machine's watch, a
// machine's stream starting or ending, or a ping.
function frame(block) {
  let type = "message";
  const data = [];
  for (const line of block.split("\n")) {
    if (line.startsWith("event:")) type = line.slice(6).trim();
    else if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
  }
  if (!data.length) return; // a comment
  heard = Date.now();
  if (type === "doc") {
    const { machine: name, doc } = JSON.parse(data.join("\n"));
    apply(name, doc);
    changed();
  } else if (type === "machine") {
    const d = JSON.parse(data.join("\n"));
    const m = machine(d.machine);
    m.up = d.up;
    m.error = d.error || "";
    render();
  }
}

let retryTimer = 0;

function reconnect(now) {
  const old = stream;
  stream = null;
  if (old) old.abort();
  st.connected = false;
  render();
  clearTimeout(retryTimer);
  retryTimer = setTimeout(connect, now ? 0 : Math.min(1000 * 2 ** retry++, 15000));
}

// A phone suspends the page and its stream; coming back, the page catches up.
document.addEventListener("visibilitychange", () => {
  render();
  if (document.visibilityState === "visible" && credential && !st.pairing && (!stream || Date.now() - heard > STALE)) {
    retry = 0;
    reconnect(true);
  }
});
setInterval(() => {
  if (stream && st.connected && Date.now() - heard > STALE) reconnect(true);
}, 10000);

// changed keeps what the page fetched in step with the sessions.
function changed() {
  const open = new Set();
  for (const x of all()) for (const q of x.s.requests) open.add(x.machine + "/" + q.id);
  for (const k of [...st.details.keys()]) if (!open.has(k)) st.details.delete(k);
  for (const k of [...st.answered]) if (!open.has(k)) st.answered.delete(k);
  render();
}

// ---- the API ----

const REASONS = {
  "refused:busy": "It is busy. Try again once it is idle.",
  "refused:changed": "It changed meanwhile: someone answered first, or the request closed.",
  "refused:unbound": "tmag cannot prove which pane shows it. Use its terminal.",
  "refused:unsupported": "Nothing carries this to it from here. Use its terminal.",
  "refused:idle": "It is not working: nothing to interrupt.",
  "refused:background": "Its terminal is running another program.",
  "refused:subagent": "Its pane shows a subagent, which takes no input.",
  "refused:self": "An agent cannot act on its own session.",
  "refused:agent": "Only a person may answer this.",
  ambiguous: "It ran several calls of this tool at once, so tmag cannot tell which one asks. Use its terminal.",
  not_found: "It is gone.",
  ended: "The session ended.",
  no_reply: "Its last turn ended without a reply.",
  timeout: "It did not answer in time.",
};

class Failure extends Error {
  constructor(code, message) {
    super(REASONS[code] || message || code);
    this.code = code;
  }
}

async function api(path, body, method) {
  const init = { cache: "no-store", headers: { [AUTHORIZATION]: "Bearer " + credential } };
  if (method) init.method = method;
  if (body) {
    if (!method) init.method = "POST";
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch {
    throw new Failure("network", "No connection to tmag-client.");
  }
  const text = await res.text();
  let doc = null;
  try { doc = JSON.parse(text); } catch { /* a plain-text error */ }
  if (res.status === 401) needPairing();
  if (!res.ok) {
    const e = doc && doc.error;
    throw e ? new Failure(e.code, e.message) : new Failure(String(res.status), text.trim());
  }
  return doc;
}

// needPairing forgets the credential, which this device lacks or which no
// longer opens anything, and asks for a code instead.
function needPairing() {
  clearTimeout(retryTimer);
  credential = "";
  localStorage.removeItem(CREDENTIAL_KEY);
  const old = stream;
  stream = null;
  if (old) old.abort();
  st.connected = false;
  st.pairing = { error: "", busy: false };
  render();
}

async function pair(code) {
  st.pairing.busy = true;
  st.pairing.error = "";
  render();
  try {
    const doc = await api("/api/pair", { code });
    setCredential(doc.credential.token);
    st.pairing = null;
    start();
  } catch (err) {
    st.pairing.busy = false;
    st.pairing.error = err.message;
    render();
  }
}

const qs = params => new URLSearchParams(params).toString();

function detail(x, q) {
  const k = x.machine + "/" + q.id;
  let d = st.details.get(k);
  if (!d) {
    d = { state: "loading" };
    st.details.set(k, d);
    api("/api/request?" + qs({ machine: x.machine, target: x.s.id, request: q.id }))
      .then(doc => { d.state = "ok"; d.doc = doc; }, err => { d.state = "error"; d.error = err; })
      .finally(render);
  }
  return d;
}

async function act(key, body, done) {
  if (st.busy.has(key)) return;
  st.busy.add(key);
  render();
  try {
    await api("/api/act", body);
    done();
  } catch (err) {
    toast(err.message, true);
  } finally {
    st.busy.delete(key);
    render();
  }
}

function answer(x, q, decision, extra) {
  const k = x.machine + "/" + q.id;
  act("answer:" + k, { machine: x.machine, verb: "answer", target: x.s.id, request: q.id, decision, ...extra }, () => {
    st.answered.add(k);
    toast(decision === "allow" ? "Allowed. It goes on." : "Denied.");
  });
}

function send(x) {
  const k = x.machine + "/" + x.s.id;
  const text = (st.drafts.get(k) || "").trim();
  if (!text) return;
  act("send:" + k, { machine: x.machine, verb: "send", target: target(x), text }, () => {
    st.drafts.delete(k);
    toast("Sent.");
  });
}

function interrupt(x) {
  const k = x.machine + "/" + x.s.id;
  if (st.armed !== k) {
    st.armed = k;
    setTimeout(() => { if (st.armed === k) { st.armed = ""; render(); } }, 3000);
    render();
    return;
  }
  st.armed = "";
  act("halt:" + k, { machine: x.machine, verb: "interrupt", target: target(x) }, () => toast("Interrupted."));
}

function reply(x) {
  const k = x.machine + "/" + x.s.id;
  let r = st.replies.get(k);
  if (!r || r.turn !== x.s.turn) {
    r = { turn: x.s.turn, state: "loading" };
    st.replies.set(k, r);
    api("/api/read?" + qs({ machine: x.machine, target: x.s.id, last: "1" }))
      .then(doc => { r.state = "ok"; r.text = doc.text || ""; }, err => { r.state = "error"; r.error = err; })
      .finally(render);
  }
  return r;
}

function screen(x, refresh) {
  const k = x.machine + "/" + x.s.id;
  let v = st.screens.get(k);
  const pane = x.panes[0];
  if (pane && (!v || refresh)) {
    v = { state: "loading", text: v && v.text };
    st.screens.set(k, v);
    api("/api/read?" + qs({ machine: x.machine, target: pane.server + ":" + pane.id, lines: "60" }))
      .then(doc => { v.state = "ok"; v.text = (doc.text || "").replace(/\s+$/, ""); v.at = Date.now(); }, err => { v.state = "error"; v.error = err; })
      .finally(render);
  }
  return v;
}

async function check() {
  const out = [];
  for (const name of st.machines) {
    let list;
    try {
      list = await api("/api/list?" + qs({ machine: name }));
    } catch (err) {
      out.push(name + ": " + err.message);
      continue;
    }
    const m = machine(name);
    const diffs = [];
    for (const s of list.sessions) {
      const got = m.sessions.get(skey(s.agent, s.id));
      if (!got) { diffs.push("missing " + short(s.id)); continue; }
      for (const f of ["state", "kind", "turn", "cwd", "runtime"]) {
        if ((got[f] || "") !== (s[f] || "")) diffs.push(short(s.id) + " " + f + ": " + (got[f] || "-") + " here, " + (s[f] || "-") + " in list");
      }
    }
    for (const s of m.sessions.values()) if (!list.sessions.some(l => l.id === s.id)) diffs.push("extra " + short(s.id));
    const panes = p => p.map(x => x.id + "=" + (x.session || "")).sort().join(" ");
    if (panes(m.panes) !== panes(list.panes)) diffs.push("panes differ");
    out.push(name + ": " + (diffs.length ? diffs.join("; ") : "matches tmag list"));
  }
  st.check = out.join("\n");
  render();
}

// ---- words ----

const short = id => (id ? id.slice(0, 8) : "-");
const base = path => (path ? path.replace(/\/+$/, "").split("/").pop() || path : "");

function ago(t) {
  if (!t) return "";
  let s = Math.max(0, Math.floor((Date.now() - Date.parse(t)) / 1000));
  if (s < 60) return s + "s";
  if ((s = Math.floor(s / 60)) < 60) return s + "m";
  if ((s = Math.floor(s / 60)) < 24) return s + "h";
  return Math.floor(s / 24) + "d";
}

function stateText(s) {
  switch (s.state) {
    case "blocked": return s.kind === "question" ? "has a question" : "waits for approval";
    case "working": return s.kind === "background" ? "working in the background" : "working";
    case "idle": return s.kind === "error" ? "stopped on an error" : "idle";
  }
  return "not reporting yet";
}

// ---- rendering ----

function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k === "text") n.textContent = v;
    else if (v !== undefined && v !== null && v !== false) n.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) if (kid !== null && kid !== undefined && kid !== false) n.append(kid);
  return n;
}

const dot = state => el("span", { class: "dot " + (state || "unknown"), "aria-hidden": "true" });

let queued = false;
function render() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => { queued = false; draw(); });
}

const view = document.getElementById("view");
let mounted = { route: null, parts: new Map() };
let focusedOnce = "";

function route() {
  if (!validRoute(location.hash)) return { view: "home" };
  const r = location.hash.match(/^#\/s\/([^/]+)(?:\/r\/([^/]+))?$/);
  return r ? { view: "session", id: decodePart(r[1]), request: r[2] ? decodePart(r[2]) : "" } : { view: "home" };
}

window.addEventListener("hashchange", () => { render(); window.scrollTo(0, 0); });

// draw rebuilds only the parts of the view whose content changed, so that a
// stream of events leaves a prompt being written, and its focus, alone.
function draw() {
  drawMachines();
  const r = st.pairing ? { view: "pair" } : route();
  const key = r.view + ":" + (r.id || "");
  if (mounted.route !== key) {
    view.replaceChildren();
    mounted = { route: key, parts: new Map() };
  }
  const parts = r.view === "pair" ? pairParts() : r.view === "session" ? sessionParts(r) : homeParts();
  const keep = new Set();
  let prev = null;
  for (const p of parts) {
    keep.add(p.id);
    let cur = mounted.parts.get(p.id);
    if (!cur || cur.sig !== p.sig) {
      const node = p.build();
      if (cur) swap(cur.node, node);
      cur = { sig: p.sig, node };
      mounted.parts.set(p.id, cur);
    }
    const next = prev ? prev.nextSibling : view.firstChild;
    if (cur.node !== next) view.insertBefore(cur.node, next);
    prev = cur.node;
  }
  for (const [id, cur] of mounted.parts) {
    if (!keep.has(id)) {
      cur.node.remove();
      mounted.parts.delete(id);
    }
  }
  if (r.request && focusedOnce !== r.request) {
    const req = view.querySelector('[data-request="' + CSS.escape(r.request) + '"]');
    if (req) {
      focusedOnce = r.request;
      req.scrollIntoView({ block: "start" });
    }
  }
}

// swap replaces a part, carrying over the field being typed in.
function swap(old, node) {
  const a = document.activeElement;
  const key = a && old.contains(a) && a.dataset.key;
  const sel = key && "selectionStart" in a ? [a.selectionStart, a.selectionEnd] : null;
  old.replaceWith(node);
  if (key) {
    const n = node.querySelector('[data-key="' + CSS.escape(key) + '"]');
    if (n) {
      n.focus({ preventScroll: true });
      if (sel) n.setSelectionRange(sel[0], sel[1]);
    }
  }
}

function drawMachines() {
  const box = document.getElementById("machines");
  let items;
  if (st.pairing) {
    items = [];
  } else if (!st.connected) {
    items = [el("span", { class: "machine down", title: st.refused }, dot("down"), st.refused ? "refused: " + st.refused : "reconnecting")];
  } else {
    const names = st.machines.length > 1 ? st.machines : st.machines.filter(n => !machine(n).up);
    items = names.map(n => {
      const m = machine(n);
      return el("span", { class: "machine" + (m.up ? "" : " down"), title: m.error || "" }, dot(m.up ? "up" : "down"), m.up ? n : n + " offline");
    });
    if (!items.length) items = [el("span", { class: "machine" }, dot("up"), "live")];
  }
  box.replaceChildren(...items);
}

// ---- pairing ----

function pairParts() {
  const p = st.pairing;
  return [{
    id: "pair",
    sig: JSON.stringify([p.error, p.busy]),
    build: () => {
      const input = el("input", {
        type: "text", autocomplete: "one-time-code", autocapitalize: "characters", autocorrect: "off", spellcheck: "false",
        maxlength: "14", "data-key": "pair-code", "aria-label": "Pairing code", placeholder: "XXXX-XXXX-XXXX",
      });
      const submit = e => { e.preventDefault(); if (input.value.trim()) pair(input.value.trim()); };
      return el("form", { class: "pair", onsubmit: submit },
        el("h1", { text: "Pair this device" }),
        el("p", { class: "empty", text: "Enter the code tmag-client auth pair prints. A code pairs one device." }),
        IOS && !STANDALONE ? el("p", { class: "note", text: "On iOS, add this page to the home screen first, then pair from there: the home-screen app keeps its own storage." }) : null,
        input,
        el("div", { class: "actions single" }, el("button", { class: "btn primary", type: "submit", disabled: p.busy }, p.busy ? "Pairing" : "Pair")),
        p.error ? el("p", { class: "note error", text: p.error }) : null);
    },
  }];
}

// ---- home ----

function homeParts() {
  const xs = all();
  const groups = { inbox: [], working: [], settled: [], unknown: [] };
  for (const x of xs) groups[sectionOf(x)].push(x);
  const parts = [];
  const labels = { inbox: "Inbox", working: "Working", settled: "Settled", unknown: "Not reporting" };
  for (const name of ["inbox", "working", "settled", "unknown"]) {
    const list = groups[name];
    if (name === "unknown" && !list.length) continue;
    parts.push({
      id: "section:" + name,
      sig: JSON.stringify([name, st.connected, list.map(x => [x.machine, x.s.id, x.s.state, x.s.kind, x.s.turn, x.s.cwd, x.s.since, isSeen(x), x.panes.map(p => p.target)])]),
      build: () => list.length
        ? el("div", {}, el("h2", { class: "section-label" }, labels[name], el("span", { class: "count", text: String(list.length) })),
          el("ul", { class: "rows" }, list.map(row)))
        : el("div", {}, el("h2", { class: "section-label" }, labels[name]),
          el("p", { class: "empty" },
            el("strong", { text: name === "inbox" ? "Nothing waits for you." : name === "working" ? "No agent is working." : "No settled sessions." }),
            name === "inbox" ? (st.connected ? "A blocked agent, or an idle one on a turn you have not seen, shows here." : "Connecting to tmag-client.") : "")),
    });
  }
  parts.push(pushPart());
  parts.push(footPart());
  return parts;
}

function row(x) {
  const where = [x.s.agent, stateText(x.s)];
  if (st.machines.length > 1) where.push(x.machine);
  if (x.panes[0]) where.push(x.panes[0].target);
  return el("li", {}, el("a", { class: "row", href: "#/s/" + encodeURIComponent(x.s.id) },
    dot(x.s.state),
    el("span", { class: "title", text: base(x.s.cwd) || short(x.s.id) }),
    el("span", { class: "sub", text: where.join(" · ") }),
    el("span", { class: "age", text: ago(waiting(x)) })));
}

// ---- push ----

const canPush = () => "Notification" in window && "serviceWorker" in navigator && "PushManager" in window;

function pushAdvice() {
  if (IOS && !STANDALONE) return "On iOS, add this page to the home screen to receive notifications.";
  if (!canPush()) return "This browser does not offer notifications.";
  if (!st.push.enabled) return st.push.reason || "Push is not available.";
  return "";
}

function keyBytes(value) {
  const s = value.replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(s + "=".repeat((4 - (s.length % 4)) % 4));
  return Uint8Array.from(bin, c => c.charCodeAt(0));
}

async function togglePush(on) {
  st.pushBusy = true;
  render();
  try {
    if (on) {
      if (Notification.permission !== "granted" && await Notification.requestPermission() !== "granted") {
        toast("Notifications are blocked in this browser's settings.", true);
        return;
      }
      const reg = await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(st.push.key) });
      const json = sub.toJSON();
      await api("/api/push", { endpoint: json.endpoint, keys: json.keys });
      st.push.subscribed = true;
      toast("Notifications are on for this device.");
    } else {
      const reg = await navigator.serviceWorker.getRegistration();
      const sub = reg && await reg.pushManager.getSubscription();
      if (sub) await sub.unsubscribe();
      await api("/api/push", null, "DELETE");
      st.push.subscribed = false;
      toast("Notifications are off for this device.");
    }
  } catch (err) {
    toast(err.message || String(err), true);
  } finally {
    st.pushBusy = false;
    render();
  }
}

function pushPart() {
  return {
    id: "push",
    sig: JSON.stringify([st.push, st.pushBusy, canPush(), IOS, STANDALONE]),
    build: () => {
      const advice = pushAdvice();
      const box = el("div", {},
        el("h2", { class: "section-label" }, "Notifications"),
        el("p", { class: "note" }, advice || (st.push.subscribed
          ? "This device gets alert notifications when an agent blocks or finishes."
          : "Turn this on to hear from an agent while this page is closed.")));
      if (!advice) {
        box.append(el("div", { class: "actions single" }, el("button", {
          class: "btn", type: "button", disabled: st.pushBusy, onclick: () => togglePush(!st.push.subscribed),
        }, st.pushBusy ? "Working" : st.push.subscribed ? "Turn off notifications" : "Turn on notifications")));
      }
      return box;
    },
  };
}

function footPart() {
  return {
    id: "foot",
    sig: st.check + st.version + st.machines.join() + (st.credential ? st.credential.id : ""),
    build: () => el("div", {},
      el("h2", { class: "section-label" }, "This page"),
      el("p", { class: "note" }, "tmag-client " + st.version + " on " + st.machines.join(", ") + ". It acts only through tmag's verbs, which check each agent's state first."),
      el("button", { class: "link", type: "button", onclick: check }, "Compare with tmag list"),
      st.check ? el("pre", { class: "screen", text: st.check }) : null),
  };
}

// ---- a request ----

function requestPart(x, q, focused) {
  const k = x.machine + "/" + q.id;
  const d = detail(x, q);
  const pick = st.choices.get(k) || {};
  const note = st.notes.get(k);
  return {
    id: "req:" + k,
    sig: JSON.stringify([d.state, d.error && d.error.message, st.answered.has(k), st.busy.has("answer:" + k), pick, note !== undefined, focused, ago(q.since), x.s.cwd]),
    build: () => requestView(x, q, d, focused),
  };
}

// asks describes what a request asks, from its tool and input.
function asks(tool, input) {
  input = input || {};
  const command = Array.isArray(input.command) ? input.command.join(" ") : input.command;
  switch (tool) {
    case "Bash":
    case "write_stdin":
      return { title: "Run a command", why: input.description || input.reason, code: command || input.chars };
    case "Edit":
      return { title: "Edit a file", path: input.file_path, diff: [[input.old_string, input.new_string]] };
    case "MultiEdit":
      return { title: "Edit a file", path: input.file_path, diff: (input.edits || []).map(e => [e.old_string, e.new_string]) };
    case "Write":
      return { title: "Write a file", path: input.file_path, diff: [["", input.content]] };
    case "NotebookEdit":
      return { title: "Edit a notebook", path: input.notebook_path, code: input.new_source };
    case "apply_patch":
      return { title: "Apply a patch", why: input.reason, code: input.patch || command || json(input) };
    case "WebFetch":
      return { title: "Fetch a page", why: input.prompt, code: input.url };
    case "WebSearch":
      return { title: "Search the web", code: input.query };
    case "AskUserQuestion":
      return { title: (input.questions || []).length > 1 ? "Answer its questions" : "Answer its question", questions: input.questions || [] };
    case "ExitPlanMode":
      return { title: "Approve its plan", prose: input.plan };
  }
  return { title: tool ? "Use " + tool : "Approve a request", code: json(input) };
}

const json = v => JSON.stringify(v, null, 2);

function diffView(pairs) {
  const lines = [];
  for (const [before, after] of pairs) {
    for (const l of String(before || "").split("\n")) if (before) lines.push(el("span", { class: "del", text: "− " + l }));
    for (const l of String(after || "").split("\n")) if (after) lines.push(el("span", { class: "add", text: "+ " + l }));
  }
  return el("pre", { class: "diff" }, lines);
}

function requestView(x, q, d, focused) {
  const k = x.machine + "/" + q.id;
  const where = [st.machines.length > 1 ? x.machine : "", base(x.s.cwd), x.panes[0] ? x.panes[0].target : ""].filter(Boolean).join(" · ");
  const head = el("div", { class: "request-head" },
    dot("blocked"),
    el("span", { class: "who", text: x.s.agent + (q.agent ? " subagent" : "") }),
    el("span", { class: "where", text: where }),
    el("span", { class: "age", text: ago(q.since) }));
  const box = el("article", { class: "request" + (focused ? " focused" : ""), "data-request": q.id, "aria-label": "Request from " + x.s.agent }, head);

  if (d.state === "loading") {
    box.append(el("h3", { text: asks(q.tool, null).title }), el("div", { class: "skeleton", "aria-label": "Loading what it asks" }));
    return box;
  }
  if (d.state === "error") {
    box.append(el("h3", { text: asks(q.tool, null).title }), el("p", { class: "note error", text: d.error.message }));
    return box;
  }
  const a = asks(d.doc.tool || q.tool, d.doc.input);
  box.append(el("h3", { text: a.title }));
  if (a.why) box.append(el("p", { class: "why", text: a.why }));
  if (a.path) box.append(el("p", { class: "path", text: a.path }));
  if (a.code) box.append(el("pre", { text: a.code }));
  if (a.diff) box.append(diffView(a.diff));
  if (a.prose) box.append(el("pre", { class: "prose", text: a.prose }));

  const pick = st.choices.get(k) || {};
  for (const question of a.questions || []) {
    const chosen = pick[question.question];
    box.append(el("div", { class: "question" },
      el("p", { text: question.question }),
      el("div", { class: "choices", role: "group", "aria-label": question.question },
        (question.options || []).map(o => el("button", {
          class: "choice", type: "button", "aria-pressed": String(chosen === o.label),
          onclick: () => { st.choices.set(k, { ...pick, [question.question]: o.label }); render(); },
        }, o.label, o.description ? el("small", { text: o.description }) : null)))));
  }

  if (st.answered.has(k)) {
    box.append(el("p", { class: "note", text: "Answered. Waiting for the agent to go on." }));
    return box;
  }
  const busy = st.busy.has("answer:" + k);
  const questions = a.questions || [];
  const complete = questions.every(qq => pick[qq.question]);
  const note = st.notes.get(k);
  if (note !== undefined) {
    box.append(el("input", {
      type: "text", "data-key": "note:" + k, value: note, placeholder: "Why not, for the agent", "aria-label": "A note for the agent with the denial",
      oninput: e => st.notes.set(k, e.target.value),
    }));
  }
  const allow = questions.length
    ? el("button", { class: "btn primary", type: "button", disabled: busy || !complete, onclick: () => answer(x, q, "allow", { answers: pick }) }, busy ? "Sending" : "Send answer")
    : el("button", { class: "btn primary", type: "button", disabled: busy, onclick: () => answer(x, q, "allow") }, busy ? "Sending" : "Allow");
  const deny = el("button", { class: "btn", type: "button", disabled: busy, onclick: () => answer(x, q, "deny", note ? { message: note } : {}) }, "Deny");
  box.append(el("div", { class: "actions" }, deny, allow));
  // Claude Code passes a note on with a denial; Codex takes none.
  if (x.s.agent === "claude" && note === undefined && !questions.length) {
    box.append(el("button", { class: "link", type: "button", onclick: () => { st.notes.set(k, ""); render(); } }, "Deny with a note"));
  }
  return box;
}

// ---- a session ----

// markSeen marks the finished turn a session view shows seen on its machine.
// The Seen event that follows reports it to every page; a failure shows here.
function markSeen(x) {
  if (document.visibilityState !== "visible" || x.s.state !== "idle" || !x.s.turn || isSeen(x)) return;
  const k = seenKey(x);
  if (st.seenAttempts.has(k)) return;
  st.seenAttempts.add(k);
  api("/api/seen", { machine: x.machine, session: x.s.id, turn: x.s.turn })
    .then(() => render(), err => { st.seenErrors.set(k, err.message); render(); });
}

const seenKey = x => tkey(x.machine, x.s.agent, x.s.id) + ":" + x.s.turn;

// seenFailure says why this page could not mark the turn seen, and offers a
// retry with the session as the next render shows it.
function seenFailurePart(x) {
  const k = seenKey(x);
  const why = st.seenErrors.get(k);
  if (!why || isSeen(x)) return null;
  return {
    id: "seen",
    sig: why,
    build: () => el("p", { class: "note error" },
      el("strong", { text: "This turn is not marked seen." }),
      " " + why + " ",
      el("button", { class: "link", type: "button", onclick: () => { st.seenErrors.delete(k); st.seenAttempts.delete(k); render(); } }, "Try again")),
  };
}

function sessionParts(r) {
  const x = find(r.id);
  if (!x) {
    return [{
      id: "gone",
      sig: "gone:" + st.connected,
      build: () => el("div", { class: "detail" },
        el("a", { class: "back", href: "#/" }, "‹ Agents"),
        el("p", { class: "empty" }, el("strong", { text: st.connected ? "This session is gone." : "Connecting." }),
          st.connected ? "It ended, or tmag has not seen it on " + st.machines.join(", ") + "." : "")),
    }];
  }
  markSeen(x);
  const parts = [headPart(x)];
  const failed = seenFailurePart(x);
  if (failed) parts.push(failed);
  if (r.request && !x.s.requests.some(q => q.id === r.request)) {
    parts.push({ id: "closed", sig: "closed", build: () => el("p", { class: "note", text: "That request was answered or closed." }) });
  }
  for (const q of x.s.requests) parts.push(requestPart(x, q, q.id === r.request));
  parts.push(actionsPart(x));
  if (x.s.state === "idle" && x.s.turn) parts.push(replyPart(x));
  if (x.panes.length) parts.push(screenPart(x));
  return parts;
}

function headPart(x) {
  const s = x.s;
  return {
    id: "head",
    sig: JSON.stringify([s.state, s.kind, s.cwd, s.turn, s.runtime, ago(s.since), x.panes.map(p => p.target + p.view), x.machine, st.machines.length]),
    build: () => {
      const facts = [["agent", s.agent], ["machine", x.machine], ["pane", x.panes.map(p => p.target + (p.view === "subagent" ? " (subagent)" : "")).join(", ") || "none"],
        ["runs", s.runtime === "service" ? "in a background service" : "in its terminal"], ["session", s.id], ["turn", s.turn ? short(s.turn) : "-"]];
      return el("div", { class: "detail" },
        el("a", { class: "back", href: "#/" }, "‹ Agents"),
        el("h1", { text: base(s.cwd) || short(s.id) }),
        el("p", { class: "state-line" }, dot(s.state), el("strong", { text: stateText(s) }), s.since ? "for " + ago(s.since) : ""),
        el("dl", { class: "facts" }, facts.map(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })])));
    },
  };
}

function actionsPart(x) {
  const k = x.machine + "/" + x.s.id;
  const caps = x.s.capabilities;
  const canSend = caps.includes("send");
  const canHalt = caps.includes("interrupt") && (x.s.state === "working" || x.s.state === "blocked");
  return {
    id: "act",
    sig: JSON.stringify([canSend, canHalt, st.busy.has("send:" + k), st.busy.has("halt:" + k), st.armed === k, x.s.state]),
    build: () => {
      const box = el("div", {});
      if (canHalt) {
        const armed = st.armed === k;
        box.append(el("div", { class: "actions single" }, el("button", {
          class: "btn " + (armed ? "armed" : "danger"), type: "button", disabled: st.busy.has("halt:" + k), onclick: () => interrupt(x),
        }, armed ? "Tap again to interrupt" : "Interrupt")));
      }
      if (canSend) {
        box.append(el("h2", { class: "section-label" }, "Next prompt"),
          el("textarea", {
            "data-key": "draft:" + k, placeholder: "Tell " + x.s.agent + " what to do next", "aria-label": "Next prompt",
            oninput: e => st.drafts.set(k, e.target.value),
          }),
          el("div", { class: "actions single" }, el("button", { class: "btn primary", type: "button", disabled: st.busy.has("send:" + k), onclick: () => send(x) },
            st.busy.has("send:" + k) ? "Sending" : "Send")));
        const area = box.querySelector("textarea");
        area.value = st.drafts.get(k) || "";
      } else if (x.s.state === "idle" && !caps.includes("send")) {
        box.append(el("p", { class: "note", text: "It takes prompts only in its terminal." }));
      }
      return box;
    },
  };
}

function replyPart(x) {
  const r = reply(x);
  return {
    id: "reply",
    sig: JSON.stringify([r.turn, r.state, r.text, r.error && r.error.message]),
    build: () => el("section", {},
      el("h2", { class: "section-label" }, "Last reply"),
      r.state === "loading" ? el("div", { class: "skeleton" })
        : r.state === "error" ? el("p", { class: "note" + (r.error.code === "no_reply" ? "" : " error"), text: r.error.message })
        : el("pre", { class: "prose", text: r.text })),
  };
}

function screenPart(x) {
  const v = screen(x, false) || { state: "loading" };
  return {
    id: "screen",
    sig: JSON.stringify([v.state, v.at, v.error && v.error.message]),
    build: () => el("section", {},
      el("div", { class: "panel-head" },
        el("h2", { class: "section-label" }, "Pane", el("span", { class: "count", text: x.panes[0].target })),
        el("button", { class: "link", type: "button", onclick: () => screen(x, true) }, v.state === "loading" ? "Reading" : "Refresh")),
      v.text ? el("pre", { class: "screen", text: v.text }) : null,
      v.state === "error" ? el("p", { class: "note error", text: v.error.message }) : null,
      !v.text && v.state === "loading" ? el("div", { class: "skeleton" }) : null),
  };
}

// ---- the toast ----

let toastTimer = 0;
function toast(text, error) {
  const t = document.getElementById("toast");
  t.textContent = text;
  t.className = "toast" + (error ? " error" : "");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, error ? 6000 : 2500);
}

// ---- start ----

async function start() {
  const opened = takeOpenRoute();
  const tapped = await takeTappedRoute();
  await takeAlertRoute(!opened && !tapped);
  const invitation = takeInvitation();
  credential = localStorage.getItem(CREDENTIAL_KEY) || "";
  if (invitation) {
    st.pairing = { error: "", busy: true };
    render();
    try {
      const doc = await api("/api/pair", { code: invitation });
      setCredential(doc.credential.token);
      st.pairing = null;
    } catch (err) {
      if (!credential) {
        st.pairing = { error: err.message, busy: false };
        render();
        return;
      }
    }
  }
  if (!credential) {
    needPairing();
    return;
  }
  try {
    const doc = await api("/api/machines");
    st.machines = doc.machines;
    st.version = doc.version;
    st.push = doc.push || st.push;
    st.credential = doc.credential || null;
    for (const n of st.machines) machine(n);
  } catch (err) {
    if (!st.pairing) view.replaceChildren(el("p", { class: "empty" }, el("strong", { text: "This page cannot reach tmag." }), err.message));
    return;
  }
  connect();
  reportVisible();
  if (st.push.enabled && canPush()) navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => {});
  render();
}

// reportVisible tells the gateway whether a notification would reach the
// person here instead. A report older than a minute counts as not visible.
function reportVisible() {
  if (!credential) return;
  api("/api/visible", { visible: document.visibilityState === "visible" }).catch(() => {});
}
document.addEventListener("visibilitychange", async () => {
  reportVisible();
  if (document.visibilityState !== "visible") return;
  const tapped = await takeTappedRoute();
  await takeAlertRoute(!tapped);
});
setInterval(() => { if (document.visibilityState === "visible") reportVisible(); }, 30000);
window.addEventListener("pagehide", () => {
  if (!credential) return;
  fetch("/api/visible", {
    method: "POST", keepalive: true,
    headers: { [AUTHORIZATION]: "Bearer " + credential, "Content-Type": "application/json" },
    body: JSON.stringify({ visible: false }),
  }).catch(() => {});
});

setInterval(render, 15000); // the ages on screen
start();
