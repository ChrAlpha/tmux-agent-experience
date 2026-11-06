// tmag serve: the agents in tmux on each machine, from a phone.
//
// Everything shown is rebuilt from each machine's `tmag watch` stream, as
// the tmag/v1 contract tells a client to: a snapshot, then every event (one
// with a state reports its session in full), and the latest pane list. Every
// action is one tmag verb, which checks its own gate on the machine.
"use strict";

const ENDED = new Set(["SessionEnd", "Exit"]);
const REPORTED = ["state", "kind", "turn", "cwd", "runtime", "transcript", "subagents"];
const ORDER = { blocked: 0, working: 1, idle: 2, unknown: 3 };
const STALE = 45000; // a stream silent this long is presumed dead

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
  m.seq = e.seq;
  const k = skey(e.agent, e.session);
  if (ENDED.has(e.event)) {
    m.sessions.delete(k);
    return;
  }
  // Only an event with a state speaks for the session as a whole; a field
  // left out is empty.
  if (!e.state) return;
  const prev = m.sessions.get(k);
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

// all is every session of every machine, with the panes that show it.
function all() {
  const out = [];
  for (const [name, m] of st.m) {
    for (const s of m.sessions.values()) {
      out.push({ machine: name, s, panes: m.panes.filter(p => p.session === s.id && p.agent === s.agent) });
    }
  }
  return out.sort((a, b) => (ORDER[a.s.state] ?? 9) - (ORDER[b.s.state] ?? 9) || String(a.s.since).localeCompare(String(b.s.since)));
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

let source = null;
let retry = 0;
let heard = 0;

function connect() {
  const since = [];
  for (const [name, m] of st.m) if (m.seq !== null) since.push(name + ":" + m.seq);
  source = new EventSource("/api/watch" + (since.length ? "?since=" + encodeURIComponent(since.join(",")) : ""));
  source.onopen = () => {
    retry = 0;
    heard = Date.now();
    st.connected = true;
    render();
  };
  source.addEventListener("doc", ev => {
    heard = Date.now();
    const { machine: name, doc } = JSON.parse(ev.data);
    apply(name, doc);
    changed();
  });
  source.addEventListener("machine", ev => {
    heard = Date.now();
    const d = JSON.parse(ev.data);
    const m = machine(d.machine);
    m.up = d.up;
    m.error = d.error || "";
    render();
  });
  source.addEventListener("ping", () => { heard = Date.now(); });
  source.onerror = () => reconnect(false);
}

function reconnect(now) {
  if (source) source.close();
  source = null;
  st.connected = false;
  render();
  setTimeout(connect, now ? 0 : Math.min(1000 * 2 ** retry++, 15000));
}

// A phone suspends the page and its stream; coming back, the page catches up.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && (!source || Date.now() - heard > STALE)) {
    retry = 0;
    reconnect(true);
  }
});
setInterval(() => {
  if (source && st.connected && Date.now() - heard > STALE) reconnect(true);
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

async function api(path, body) {
  const init = { credentials: "same-origin", cache: "no-store" };
  if (body) {
    init.method = "POST";
    init.headers = { "Content-Type": "application/json", "X-Tmag-Action": "1" };
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch {
    throw new Failure("network", "No connection to tmag serve.");
  }
  const text = await res.text();
  let doc = null;
  try { doc = JSON.parse(text); } catch { /* a plain-text error */ }
  if (!res.ok) {
    const e = doc && doc.error;
    throw e ? new Failure(e.code, e.message) : new Failure(String(res.status), text.trim());
  }
  return doc;
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
  const r = location.hash.match(/^#\/s\/([^/]+)(?:\/r\/([^/]+))?/);
  return r ? { view: "session", id: decodeURIComponent(r[1]), request: r[2] ? decodeURIComponent(r[2]) : "" } : { view: "home" };
}

window.addEventListener("hashchange", () => { render(); window.scrollTo(0, 0); });

// draw rebuilds only the parts of the view whose content changed, so that a
// stream of events leaves a prompt being written, and its focus, alone.
function draw() {
  drawMachines();
  const r = route();
  const key = r.view + ":" + (r.id || "");
  if (mounted.route !== key) {
    view.replaceChildren();
    mounted = { route: key, parts: new Map() };
  }
  const parts = r.view === "session" ? sessionParts(r) : homeParts();
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
  if (!st.connected) {
    items = [el("span", { class: "machine down" }, dot("down"), "reconnecting")];
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

// ---- home ----

function homeParts() {
  const xs = all();
  const parts = [];
  const asks = [];
  for (const x of xs) for (const q of x.s.requests) asks.push({ x, q });
  asks.sort((a, b) => String(a.q.since).localeCompare(String(b.q.since)));

  parts.push({
    id: "needs-label",
    sig: "n" + asks.length + ":" + st.connected + ":" + xs.length,
    build: () => asks.length
      ? el("h2", { class: "section-label" }, "Needs you", el("span", { class: "count", text: String(asks.length) }))
      : el("div", {}, el("h2", { class: "section-label" }, "Needs you"),
        el("p", { class: "empty" }, el("strong", { text: "Nothing waits for you." }),
          st.connected ? "When an agent asks to run something, or asks you a question, it shows here." : "Connecting to tmag serve.")),
  });
  for (const { x, q } of asks) parts.push(requestPart(x, q, false));

  parts.push({
    id: "agents-label",
    sig: "a" + xs.length,
    build: () => el("h2", { class: "section-label" }, "Agents", el("span", { class: "count", text: String(xs.length) })),
  });
  parts.push({
    id: "agents",
    sig: JSON.stringify(xs.map(x => [x.machine, x.s.id, x.s.state, x.s.kind, x.s.cwd, ago(x.s.since), x.panes.map(p => p.target)])) + st.machines.length,
    build: () => xs.length ? el("ul", { class: "rows" }, xs.map(row)) : el("p", { class: "empty" },
      el("strong", { text: "No agents yet." }),
      "Start Claude Code or Codex in a tmux pane on " + (st.machines.join(", ") || "this machine") + ". It shows here from its first hook."),
  });
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
    el("span", { class: "age", text: ago(x.s.since) })));
}

function footPart() {
  return {
    id: "foot",
    sig: st.check + st.version + st.machines.join(),
    build: () => el("div", {},
      el("h2", { class: "section-label" }, "This page"),
      el("p", { class: "note" }, "tmag " + st.version + " on " + st.machines.join(", ") + ". It acts only through tmag's verbs, which check each agent's state first."),
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
  const parts = [headPart(x)];
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
  const canSend = caps.includes("send") && x.s.state === "idle";
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
  try {
    const doc = await api("/api/machines");
    st.machines = doc.machines;
    st.version = doc.version;
    for (const n of st.machines) machine(n);
  } catch (err) {
    view.replaceChildren(el("p", { class: "empty" }, el("strong", { text: "This page cannot reach tmag." }), err.message));
    return;
  }
  connect();
  render();
  setInterval(render, 15000); // the ages on screen
}

start();
