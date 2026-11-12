// tmag-client's page: the inbox of one person's agents, each session with its
// last reply, its pane and its open requests, and what can be done there:
// answer, prompt, stop. Everything comes from the gateway's event stream and
// API; the page keeps its credential and nothing an agent wrote.
"use strict";

const TOKEN_KEY = "tmag-token";
const IOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
const STANDALONE = matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
const STALE = 40000; // a stream this quiet is dead: the gateway pings every 15 s

const state = {
  token: localStorage.getItem(TOKEN_KEY) || "",
  device: null,
  version: "",
  multi: false,
  machines: new Map(),
  sessions: new Map(),
  live: false,
  push: null,
};
const replies = new Map(); // key → {turn, text} or {error}, in memory only
const drafts = new Map(); // key → a prompt not sent yet, in memory only

// ---- DOM

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid != null && kid !== false) el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}

// put replaces an element's children with the kids given, leaving out
// those that are null or false.
function put(el, ...kids) {
  el.replaceChildren(...kids.flat(Infinity).filter(k => k != null && k !== false));
}

const ICONS = {
  back: "M15 5 8 12l7 7",
  gear: "M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Zm7.4-2.1.1-1.4-.1-1.4 2-1.6-2-3.4-2.4 1a7.6 7.6 0 0 0-2.4-1.4L14.2 2h-4l-.4 2.6a7.6 7.6 0 0 0-2.4 1.4l-2.4-1-2 3.4 2 1.6-.1 1.4.1 1.4-2 1.6 2 3.4 2.4-1a7.6 7.6 0 0 0 2.4 1.4l.4 2.6h4l.4-2.6a7.6 7.6 0 0 0 2.4-1.4l2.4 1 2-3.4Z",
  send: "M12 19V5m-6 6 6-6 6 6",
  stop: "M7 7h10v10H7z",
  refresh: "M20 11a8 8 0 1 0-2.3 5.7M20 5v6h-6",
};

function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", ICONS[name]);
  path.setAttribute("fill", name === "stop" ? "currentColor" : "none");
  path.setAttribute("stroke", "currentColor");
  path.setAttribute("stroke-width", "2");
  path.setAttribute("stroke-linecap", "round");
  path.setAttribute("stroke-linejoin", "round");
  svg.append(path);
  return svg;
}

function ago(t) {
  if (!t) return "";
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000);
  if (s < 45) return "now";
  if (s < 3600) return Math.round(s / 60) + "m";
  if (s < 86400) return Math.round(s / 3600) + "h";
  return Math.round(s / 86400) + "d";
}

const base = path => (path ? path.replace(/\/+$/, "").split("/").pop() || path : "");
const short = id => (id || "").slice(0, 8);
const name = s => base(s.cwd) || short(s.id);
const can = (s, verb) => (s.capabilities || []).includes(verb);
const visible = () => document.visibilityState === "visible";

// ---- the gateway's API

class Failure extends Error {
  constructor(code, message, next) {
    super(message);
    this.code = code;
    this.next = next || "";
  }
}

async function api(method, path, body) {
  const headers = { Authorization: "Bearer " + state.token };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let r;
  try {
    r = await fetch(path, { method, headers, cache: "no-store", body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new Failure("offline", "The gateway cannot be reached.");
  }
  if (r.status === 401) {
    unpaired("This device is no longer paired.");
    throw new Failure("unauthorized", "This device is no longer paired.");
  }
  if (r.status === 204) return null;
  let doc = null;
  try { doc = await r.json(); } catch { /* not JSON */ }
  if (!r.ok) {
    const e = (doc && doc.error) || {};
    throw new Failure(e.code || "error", e.message || r.status + " " + r.statusText, e.next);
  }
  return doc;
}

// ---- the event stream

let stream = null; // { ctl, heard }
let retry = 0;
let retryTimer = 0;
let incoming = null; // the sessions of a stream until it is ready

const on = {
  hello(d) {
    state.device = d.device;
    state.version = d.version;
    state.multi = d.machines > 1;
    state.machines.clear();
    incoming = new Map();
  },
  machine(d) { state.machines.set(d.name, d); changed(); },
  session(d) {
    if (incoming) incoming.set(d.key, d);
    else { state.sessions.set(d.key, d); changed(); }
  },
  gone(d) { state.sessions.delete(d.key); changed(); },
  ready() {
    if (incoming) state.sessions = incoming;
    incoming = null;
    retry = 0;
    state.live = true;
    changed();
  },
  alert(n) { alerted(n); },
};

async function connect() {
  if (!state.token || stream) return;
  clearTimeout(retryTimer);
  const me = { ctl: new AbortController(), heard: Date.now() };
  stream = me;
  try {
    const r = await fetch("/api/events", { headers: { Authorization: "Bearer " + state.token }, signal: me.ctl.signal, cache: "no-store" });
    if (r.status === 401) {
      unpaired("This device is no longer paired.");
      return;
    }
    if (!r.ok || !r.body) throw new Error(String(r.status));
    const reader = r.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      me.heard = Date.now();
      buf += decoder.decode(value, { stream: true });
      let end;
      while ((end = buf.indexOf("\n\n")) >= 0) {
        frame(buf.slice(0, end));
        buf = buf.slice(end + 2);
      }
    }
  } catch {
    /* aborted, or the connection broke */
  } finally {
    if (stream === me) {
      stream = null;
      incoming = null;
      state.live = false;
      changed();
      if (state.token) {
        const wait = Math.min(30000, 1000 * 2 ** retry++) * (0.8 + Math.random() * 0.4);
        retryTimer = setTimeout(connect, wait);
      }
    }
  }
}

function frame(block) {
  let event = "message";
  let data = "";
  for (const line of block.split("\n")) {
    if (!line || line.startsWith(":")) continue;
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    else if (field === "data") data += (data ? "\n" : "") + value;
  }
  if (!data || !on[event]) return;
  try { on[event](JSON.parse(data)); } catch { /* a frame this page does not read */ }
}

// A page coming back on screen checks its stream at once: a phone freezes
// pages in the background, and their connections die without a word.
function revive() {
  if (!state.token) return;
  if (stream && Date.now() - stream.heard > 20000) stream.ctl.abort();
  if (!stream) {
    retry = 0;
    connect();
  }
}

setInterval(() => {
  if (stream && Date.now() - stream.heard > STALE) stream.ctl.abort();
}, 5000);

// ---- presence: a page on screen gets alerts in place, not pushed

function present() {
  if (!state.token) return;
  const body = JSON.stringify({ visible: visible() });
  fetch("/api/presence", { method: "POST", keepalive: true, headers: { Authorization: "Bearer " + state.token, "Content-Type": "application/json" }, body }).catch(() => {});
}

document.addEventListener("visibilitychange", () => {
  present();
  if (visible()) {
    revive();
    changed();
  }
});
setInterval(() => { if (visible()) present(); }, 20000);
setInterval(changed, 30000); // ages on screen

// ---- the router

function route() {
  const hash = location.hash || "#/";
  if (hash.startsWith("#pair=") && !state.token) return { name: "pair", code: decodeURIComponent(hash.slice(6)) };
  if (!state.token) return { name: "pair", code: "" };
  if (hash.startsWith("#pair=")) history.replaceState(null, "", "/#/"); // paired already
  if (hash.startsWith("#/s/")) return { name: "session", key: hash.slice(4) };
  if (hash === "#/settings") return { name: "settings" };
  return { name: "inbox" };
}

const views = { inbox: inboxView, session: sessionView, settings: settingsView, pair: pairView };
const main = document.getElementById("view");
const bar = document.getElementById("bar");
let view = null;

function show() {
  const r = route();
  if (view && view.name === r.name && view.key === r.key) {
    view.update();
    return;
  }
  if (view && view.destroy) view.destroy();
  view = Object.assign(views[r.name](r), { name: r.name, key: r.key });
  put(main, view.el);
  scrollTo(0, 0);
  view.update();
}

let queued = false;
function changed() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => {
    queued = false;
    if (view) view.update();
  });
}

function drawBar(title, sub, back) {
  const live = h("span", { class: state.live ? "live" : "live down", title: state.live ? "Live" : "Reconnecting" }, state.live ? "" : "Reconnecting");
  put(bar, 
    back ? h("a", { class: "icon-button", href: "#/", "aria-label": "Back to the inbox" }, icon("back")) : null,
    h("div", { class: "title" }, h("strong", {}, title), sub ? h("span", {}, sub) : null),
    state.token ? live : null,
    state.token && !back ? h("a", { class: "icon-button", href: "#/settings", "aria-label": "Settings" }, icon("gear")) : null,
  );
}

// ---- the inbox

// bucket is where a session waits: on the person, at work, settled, or on a
// machine the gateway cannot reach, whose state may be stale.
function bucket(s) {
  const m = state.machines.get(s.machine);
  if (!m || m.status !== "online") return "unreachable";
  if (s.state === "blocked" || (s.state === "idle" && s.unread)) return "needs";
  if (s.state === "working") return "working";
  if (s.state === "idle") return "idle";
  return "unknown";
}

const SECTIONS = [
  ["needs", "Needs you"],
  ["working", "Working"],
  ["idle", "Idle"],
  ["unknown", "State unknown"],
  ["unreachable", "Unreachable"],
];

function sorted(list, kind) {
  const since = s => new Date(s.since || 0).getTime();
  return list.sort((a, b) => {
    if (kind === "needs" && (a.state === "blocked") !== (b.state === "blocked")) return a.state === "blocked" ? -1 : 1;
    // What waits on the person or works: longest first. What is settled:
    // latest first.
    return kind === "needs" || kind === "working" ? since(a) - since(b) : since(b) - since(a);
  });
}

function what(s) {
  if (s.state === "blocked") {
    const reqs = [...(s.requests || [])].sort((a, b) => new Date(a.since) - new Date(b.since));
    const r = reqs[0];
    let text = "Needs your answer";
    if (r && r.kind === "question") text = "Has a question";
    else if (r && r.kind === "plan") text = "Has a plan for you";
    else if (r) text = "Wants to use " + (r.tool || "a tool");
    return reqs.length > 1 ? text + " · " + (reqs.length - 1) + " more" : text;
  }
  if (s.state === "idle" && s.unread) return s.kind === "error" ? "Stopped on an error" : "Finished";
  if (s.state === "working") {
    let text = s.kind === "background" ? "Background tasks" : "Working";
    const n = (s.subagents || []).length;
    if (n) text += " · " + n + (n === 1 ? " subagent" : " subagents");
    return text;
  }
  if (s.state === "idle") return s.kind === "error" ? "Idle after an error" : "Idle";
  return "State unknown";
}

function mark(s) {
  if (s.state === "blocked") return "blocked";
  if (s.state === "idle" && s.unread) return "unread";
  if (s.state === "working") return "working";
  return "idle";
}

function row(s) {
  const kind = bucket(s);
  const cls = "row" + (s.state === "blocked" && kind === "needs" ? " needs" : s.unread && kind === "needs" ? " unread" : "");
  return h("a", { class: cls, href: "#/s/" + s.key },
    h("span", { class: "mark " + mark(s), "aria-hidden": "true" }),
    h("span", { class: "body" },
      h("span", { class: "name" }, h("b", {}, name(s)), h("span", { class: "agent" }, s.agent)),
      h("span", { class: "what" }, what(s))),
    h("span", { class: "side" }, h("span", {}, ago(s.since)), state.multi ? h("span", { class: "tag" }, s.machine) : null));
}

function machineNotes() {
  const notes = [];
  for (const m of state.machines.values()) {
    if (m.status === "online") continue;
    const text = m.status === "connecting" ? "connecting…" : "unreachable since " + ago(m.since) + (m.error ? ": " + m.error : "");
    notes.push(h("div", { class: "machine-note" }, h("b", {}, m.name), h("span", {}, text)));
  }
  return notes.length ? h("div", { class: "machines" }, notes) : null;
}

function inboxView() {
  const el = h("div", { class: "page" });
  return {
    el,
    update() {
      drawBar("tmag", "");
      document.title = "tmag";
      const groups = {};
      for (const s of state.sessions.values()) (groups[bucket(s)] ||= []).push(s);
      const parts = [machineNotes()];
      for (const [kind, title] of SECTIONS) {
        const list = groups[kind];
        if (!list || !list.length) continue;
        parts.push(h("h2", { class: "section" }, title, h("span", { class: "count" }, list.length)));
        parts.push(h("div", { class: "list" }, sorted(list, kind).map(row)));
      }
      if (!state.sessions.size) {
        const followed = state.machines.size > 0 && [...state.machines.values()].every(m => m.status === "online");
        if (!state.live) parts.push(h("p", { class: "empty" }, "Connecting to the gateway…"));
        else if (followed) parts.push(h("p", { class: "empty" }, h("strong", {}, "No agents yet"), "Start Claude Code, Codex or Pi in a tmux pane, and it shows up here."));
      }
      const needs = (groups.needs || []).length;
      document.title = needs ? "(" + needs + ") tmag" : "tmag";
      put(el, parts);
    },
  };
}

// ---- a session

function sessionView(r) {
  const key = r.key;
  const facts = h("div", { class: "facts" });
  const replyCard = h("section", { class: "card" });
  const screen = h("pre", {});
  const screenNote = h("span", { class: "note" });
  const refresh = h("button", { class: "small-button", type: "button", onclick: e => { e.preventDefault(); loadScreen(true); } }, "Refresh");
  const screenCard = h("details", { class: "card screen" },
    h("summary", {}, "Screen", screenNote, h("span", { class: "grow" }), refresh), screen);
  const dock = h("footer", { class: "dock" });
  const gone = h("p", { class: "empty" }, h("strong", {}, "This session ended"), h("a", { href: "#/" }, "Back to the inbox"));
  const page = h("div", { class: "page" }, facts, replyCard, screenCard);
  const el = h("div", { class: "session" }, page, dock);
  let last = null; // the session as last drawn
  let panel = null; // the dock's request panel or composer
  let screenTimer = 0;
  let screenBusy = false;
  let screenFollow = true;
  let openedOnce = false;
  let askedFor = ""; // the idle turn the reply was last asked for
  let replyFor = ""; // the idle turn the reply on screen was read for

  screen.addEventListener("scroll", () => {
    screenFollow = screen.scrollTop + screen.clientHeight >= screen.scrollHeight - 8;
  });
  screenCard.addEventListener("toggle", () => { if (screenCard.open) loadScreen(true); });

  // loadReply reads the last reply, once more a moment later when a turn
  // just ended and its reply is not in the transcript yet.
  async function loadReply(s, again) {
    const want = askedFor;
    try {
      const doc = await api("GET", "/api/reply?key=" + encodeURIComponent(key));
      replies.set(key, { turn: doc.turn, text: doc.text || "" });
      if (s.state === "idle" && doc.turn !== s.turn && !again) {
        setTimeout(() => { if (askedFor === want) loadReply(s, true); }, 1500);
        changed();
        return;
      }
    } catch (e) {
      if (e.code === "unauthorized") return;
      replies.set(key, { turn: "", error: e });
    }
    if (askedFor === want) replyFor = want;
    changed();
  }

  async function loadScreen(now) {
    const s = state.sessions.get(key);
    if (!s || !screenCard.open || screenBusy || (!now && !visible())) return;
    screenBusy = true;
    try {
      const doc = await api("GET", "/api/screen?lines=150&key=" + encodeURIComponent(key));
      screen.textContent = doc.text.replace(/\s+$/, "");
      screenNote.textContent = doc.target ? "· " + doc.target : "";
      if (screenFollow) screen.scrollTop = screen.scrollHeight;
    } catch (e) {
      screen.textContent = e.message;
    } finally {
      screenBusy = false;
    }
  }

  // drawReply shows the reply of the turn the session is idle on; while a
  // turn runs, tmag has no reply for it, and the card keeps the one before,
  // if this page read it.
  function drawReply(s) {
    const got = replies.get(key);
    const current = got && !got.error && got.turn === s.turn && s.state === "idle";
    replyCard.hidden = s.state !== "idle" && (!got || !!got.error);
    replyCard.classList.toggle("dim", !!got && !current);
    const note = got && !got.error && !current ? "· an earlier turn" : "";
    let content;
    if (!got) content = h("p", { class: "muted" }, "Reading…");
    else if (got.error) content = h("p", { class: "muted" }, got.error.code === "no_reply" ? "No reply yet." : got.error.message);
    else if (!got.text.trim()) content = h("p", { class: "muted" }, "The reply has no text.");
    else content = markdown(got.text);
    put(replyCard, h("header", {}, "Last reply", h("span", { class: "note" }, note)), h("div", { class: "content md" }, content));
  }

  // read marks an idle turn read once its reply is on screen.
  function read(s) {
    if (!s.unread || s.state !== "idle" || !visible() || replyFor !== "idle:" + s.turn) return;
    api("POST", "/api/read", { key, turn: s.turn }).catch(() => {});
    closeNotifications(key);
  }

  function drawDock(s) {
    const reqs = [...(s.requests || [])].sort((a, b) => new Date(a.since) - new Date(b.since));
    const want = reqs.length && can(s, "answer") ? "request:" + reqs[0].id : can(s, "send") ? "composer" : "none";
    if (!panel || panel.kind !== want) {
      if (panel && panel.destroy) panel.destroy();
      if (want.startsWith("request:")) panel = requestPanel(s, reqs[0]);
      else if (want === "composer") panel = composer(s);
      else panel = { kind: "none", el: h("p", { class: "why" }, "tmag has no way to this session's input from here."), update() {} };
      panel.kind = want;
      put(dock, panel.el);
    }
    panel.update(s, reqs);
  }

  return {
    el,
    destroy() {
      clearInterval(screenTimer);
      if (panel && panel.destroy) panel.destroy();
    },
    update() {
      const s = state.sessions.get(key);
      if (!s) {
        drawBar(last ? name(last) : "Session", last ? last.agent : "", true);
        put(el, h("div", { class: "page" }, state.live || last ? gone : h("p", { class: "empty" }, "Connecting to the gateway…")));
        if (last) clearInterval(screenTimer);
        return;
      }
      if (!el.contains(page)) put(el, page, dock);
      drawBar(name(s), s.agent, true);
      document.title = name(s) + " · tmag";
      const m = state.machines.get(s.machine);
      const offline = m && m.status !== "online";
      put(facts, 
        h("span", { class: "state" }, h("span", { class: "mark " + mark(s) }), offline ? "Unreachable" : what(s)),
        h("span", {}, ago(s.since)),
        state.multi || offline ? h("span", { class: "tag" }, s.machine) : null,
        (s.panes || [])[0] ? h("span", {}, s.panes[0].target) : null,
        s.runtime === "service" ? h("span", {}, "background service") : null,
        h("span", { title: s.id }, short(s.id)));
      if (!openedOnce) {
        openedOnce = true;
        screenCard.open = s.state !== "idle" && (s.panes || []).length > 0;
        screenTimer = setInterval(() => {
          const now = state.sessions.get(key);
          if (now && now.state !== "idle") loadScreen(false);
        }, 2500);
      }
      screenCard.hidden = !(s.panes || []).length;
      // The reply is read for each turn the session is idle on.
      if (s.state === "idle" && askedFor !== "idle:" + s.turn) {
        askedFor = "idle:" + s.turn;
        loadReply(s);
      }
      if (last && last.state !== s.state && screenCard.open) loadScreen(true);
      drawReply(s);
      drawDock(s);
      read(s);
      last = s;
    },
  };
}

// closeNotifications removes this device's notifications of a session once
// the person has seen it.
async function closeNotifications(key) {
  if (!("serviceWorker" in navigator) || !isSecureContext) return;
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    if (!reg) return;
    for (const n of await reg.getNotifications()) if (n.data && n.data.key === key) n.close();
  } catch { /* nothing to close */ }
}

function failureNode(e) {
  return h("p", { class: "error", role: "alert" }, e.message, e.next ? h("small", {}, e.next) : null);
}

function stopButton(s) {
  return h("button", {
    class: "round stop", type: "button", "aria-label": "Stop the turn", title: "Stop",
    onclick: async ev => {
      const b = ev.currentTarget;
      b.disabled = true;
      try {
        await api("POST", "/api/interrupt", { key: s.key });
        toast("Stopped");
      } catch (e) {
        toast(e.message, e.next, true);
      } finally {
        b.disabled = false;
      }
    },
  }, icon("stop"));
}

function composer(s) {
  const key = s.key;
  const area = h("textarea", { rows: "1", enterkeyhint: "send", "aria-label": "Prompt" });
  area.value = drafts.get(key) || "";
  const send = h("button", { class: "round send", type: "submit", "aria-label": "Send" }, icon("send"));
  const stopSlot = h("span", {});
  const form = h("form", { class: "composer" }, stopSlot, area, send);
  let busy = false;
  const fit = () => {
    area.style.height = "auto";
    area.style.height = Math.min(area.scrollHeight + 2, innerHeight * 0.4) + "px";
    send.disabled = busy || !area.value.trim();
  };
  area.addEventListener("input", () => { drafts.set(key, area.value); fit(); });
  area.addEventListener("keydown", e => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      form.requestSubmit();
    }
  });
  form.addEventListener("submit", async e => {
    e.preventDefault();
    const text = area.value;
    if (!text.trim() || busy) return;
    busy = true;
    fit();
    const working = (state.sessions.get(key) || s).state !== "idle";
    try {
      await api("POST", "/api/send", { key, text });
      drafts.delete(key);
      area.value = "";
      toast(working ? "Sent: it reads it at its next step" : "Sent");
    } catch (err) {
      toast(err.message, err.next, true);
    } finally {
      busy = false;
      fit();
    }
  });
  requestAnimationFrame(fit);
  return {
    el: form,
    update(now) {
      area.placeholder = "Message " + now.agent;
      const stoppable = now.state === "working" && can(now, "interrupt");
      if (stoppable !== !!stopSlot.firstChild) put(stopSlot, stoppable ? stopButton(now) : "");
    },
  };
}

// ---- requests: an approval, a question or a plan, in the agent's terms

function requestPanel(s, req) {
  const el = h("div", { class: "request" });
  let asked = null; // {tool, input} once read
  let failure = null;
  let sent = false;
  let busy = false;
  let denying = false;
  let page = 0;
  const answers = {}; // question → {picked: Set, other: string}
  const note = h("textarea", { placeholder: "Tell " + s.agent + " what to do instead", "aria-label": "Note" });
  let current = s;
  let total = 1;

  api("GET", "/api/request?key=" + encodeURIComponent(s.key) + "&request=" + encodeURIComponent(req.id))
    .then(d => { asked = d; draw(); })
    .catch(e => { failure = e; draw(); });

  async function decide(decision, extra) {
    if (busy) return;
    busy = true;
    failure = null;
    draw();
    try {
      await api("POST", "/api/answer", Object.assign({ key: s.key, request: req.id, decision }, extra || {}));
      sent = true;
      closeNotifications(s.key);
    } catch (e) {
      failure = e; // the request stays answerable
    } finally {
      busy = false;
      draw();
    }
  }

  function head(title) {
    return h("div", { class: "head" },
      h("strong", {}, title),
      total > 1 ? h("span", { class: "muted" }, "1 of " + total) : null,
      can(current, "interrupt") ? stopButton(current) : null);
  }

  function draw() {
    const kind = req.kind;
    const parts = [];
    if (kind === "question") parts.push(head(s.agent + " asks"));
    else if (kind === "plan") parts.push(head(s.agent + " has a plan"));
    else parts.push(head(s.agent + " wants to " + verb(req.tool)));
    if (!asked && !failure) parts.push(h("p", { class: "muted" }, "Reading the request…"));
    if (asked) {
      if (kind === "question" && Array.isArray(asked.input && asked.input.questions)) parts.push(...question(asked.input.questions));
      else if (kind === "plan") parts.push(h("div", { class: "detail md" }, markdown(String((asked.input && asked.input.plan) || ""))));
      else parts.push(h("div", { class: "detail" }, describe(asked.tool || req.tool, asked.input)));
    }
    if (failure) parts.push(failureNode(failure));
    if (sent) parts.push(h("p", { class: "muted" }, "Answered. Waiting for " + s.agent + "…"));
    else if ((asked || failure) && !(kind === "question" && asked && Array.isArray(asked.input && asked.input.questions))) parts.push(...choices(kind));
    put(el, ...parts);
  }

  function choices(kind) {
    const allowText = kind === "plan" ? "Approve" : "Allow";
    const denyText = kind === "plan" ? "Keep planning" : "Deny";
    if (denying) {
      return [note, h("div", { class: "actions" },
        h("button", { class: "button", type: "button", disabled: busy, onclick: () => { denying = false; draw(); } }, "Back"),
        h("button", { class: "button danger", type: "button", disabled: busy, onclick: () => decide("deny", note.value.trim() ? { message: note.value.trim() } : {}) }, kind === "plan" ? "Send feedback" : "Deny"))];
    }
    return [
      h("div", { class: "actions" },
        h("button", { class: "button danger", type: "button", disabled: busy, onclick: () => decide("deny") }, denyText),
        h("button", { class: "button primary", type: "button", disabled: busy, onclick: () => decide("allow") }, allowText)),
      h("button", { class: "link", type: "button", onclick: () => { denying = true; draw(); requestAnimationFrame(() => note.focus()); } },
        kind === "plan" ? "Keep planning, with feedback…" : "Deny with a note…"),
    ];
  }

  // question shows one question at a time; the last page sends every answer.
  function question(questions) {
    const q = questions[Math.min(page, questions.length - 1)];
    const a = (answers[q.question] ||= { picked: new Set(), other: "" });
    const multi = !!q.multiSelect;
    const other = h("input", { type: "text", placeholder: "Something else", "aria-label": "Another answer", value: a.other });
    other.addEventListener("input", () => {
      a.other = other.value;
      if (!multi && a.other) a.picked.clear();
      refreshButtons();
    });
    const options = (q.options || []).map(o => {
      const b = h("button", { class: "option", type: "button", "aria-pressed": a.picked.has(o.label) ? "true" : "false" },
        h("b", {}, o.label), o.description ? h("span", {}, o.description) : null);
      b.addEventListener("click", () => {
        if (multi) a.picked.has(o.label) ? a.picked.delete(o.label) : a.picked.add(o.label);
        else {
          a.picked = new Set([o.label]);
          a.other = "";
          other.value = "";
        }
        for (const x of opts.children) x.setAttribute("aria-pressed", a.picked.has(x.firstChild.textContent) ? "true" : "false");
        refreshButtons();
      });
      return b;
    });
    const opts = h("div", { class: "options" }, options);
    const answered = x => x && (x.picked.size > 0 || x.other.trim() !== "");
    const lastPage = page >= questions.length - 1;
    const back = h("button", { class: "button", type: "button", onclick: () => { page--; draw(); } }, "Back");
    const next = h("button", { class: "button primary", type: "button" }, lastPage ? "Send answers" : "Next");
    function refreshButtons() { next.disabled = busy || sent || !answered(a); }
    next.addEventListener("click", () => {
      if (!lastPage) {
        page++;
        draw();
        return;
      }
      const out = {};
      for (const x of questions) {
        const got = answers[x.question];
        if (!answered(got)) {
          page = questions.indexOf(x);
          draw();
          return;
        }
        out[x.question] = [...got.picked, got.other.trim()].filter(Boolean).join(", ");
      }
      decide("allow", { answers: out });
    });
    refreshButtons();
    const header = h("div", { class: "question-head" }, q.header ? h("span", { class: "tag" }, q.header) : null,
      questions.length > 1 ? h("span", { class: "muted" }, "Question " + (page + 1) + " of " + questions.length) : null,
      multi ? h("span", { class: "muted" }, "Choose any") : null);
    return [
      h("div", { class: "detail" }, header, h("p", { class: "lead" }, h("b", {}, q.question)), opts, other),
      sent ? null : h("div", { class: "actions" }, page > 0 ? back : null, next),
      sent ? null : h("button", { class: "link", type: "button", onclick: () => decide("deny") }, "Decline to answer"),
    ];
  }

  return {
    el,
    update(now, reqs) {
      current = now;
      if (reqs.length !== total) {
        total = reqs.length;
        draw();
      }
    },
  };
}

function verb(tool) {
  switch (tool) {
    case "Bash": case "shell": case "exec_command": case "local_shell": case "bash": return "run a command";
    case "Edit": case "MultiEdit": case "edit": case "apply_patch": return "edit a file";
    case "Write": case "write": return "write a file";
    case "NotebookEdit": return "edit a notebook";
    case "Read": case "read": return "read a file";
    case "WebFetch": return "fetch a page";
    case "WebSearch": return "search the web";
    case "Task": case "Agent": return "start a subagent";
    default: return tool ? "use " + tool : "go on";
  }
}

// describe shows a tool's input as the agent will act on it: the command, the
// file and its change, the address; any other input as its fields.
function describe(tool, input) {
  const v = input && typeof input === "object" ? input : {};
  const str = x => (typeof x === "string" ? x : "");
  const out = [];
  const command = Array.isArray(v.command) ? v.command.join(" ") : str(v.command) || str(v.cmd);
  if (command) {
    if (str(v.description)) out.push(h("p", { class: "lead" }, v.description));
    out.push(h("pre", { class: "command" }, command));
    const where = str(v.cwd) || str(v.workdir);
    if (where) out.push(h("span", { class: "path" }, "in " + where));
    return out;
  }
  const path = str(v.file_path) || str(v.path) || str(v.notebook_path);
  if (path) out.push(h("span", { class: "path" }, path));
  if (typeof v.old_string === "string" && typeof v.new_string === "string") {
    out.push(diff(v.old_string, v.new_string));
    return out;
  }
  if (Array.isArray(v.edits)) {
    for (const e of v.edits) if (e && typeof e.old_string === "string") out.push(diff(e.old_string, String(e.new_string || "")));
    return out;
  }
  if (typeof v.content === "string") {
    out.push(clipped(v.content));
    return out;
  }
  const patch = str(v.patch) || str(v.input);
  if (patch && /^(\*\*\* Begin Patch|diff |--- |@@ )/m.test(patch)) {
    out.push(h("pre", { class: "diff" }, patch.split("\n").map(l =>
      h("span", { class: l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : "" }, l || " "))));
    return out;
  }
  if (str(v.url)) out.push(h("pre", {}, v.url));
  if (str(v.query)) out.push(h("pre", {}, v.query));
  if (str(v.prompt)) out.push(h("p", { class: "lead" }, v.prompt));
  if (out.length) return out;
  const fields = Object.entries(v);
  if (!fields.length) return [h("p", { class: "muted" }, "No details.")];
  return [h("dl", {}, fields.map(([k, x]) => [h("dt", {}, k), h("dd", {}, typeof x === "string" && !x.includes("\n") ? x : h("pre", {}, typeof x === "string" ? x : JSON.stringify(x, null, 2)))]))];
}

function clipped(text) {
  const lines = text.split("\n");
  const shown = lines.slice(0, 80).join("\n");
  return h("pre", {}, shown + (lines.length > 80 ? "\n… " + (lines.length - 80) + " more lines" : ""));
}

// diff shows the lines an edit removes and adds, with three lines of context.
function diff(before, after) {
  const a = before.split("\n");
  const b = after.split("\n");
  if (a.length * b.length > 250000) {
    return h("pre", { class: "diff" }, a.map(l => h("span", { class: "del" }, "- " + l)), b.map(l => h("span", { class: "add" }, "+ " + l)));
  }
  const n = a.length;
  const m = b.length;
  const lcs = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) lcs[i][j] = a[i] === b[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  const ops = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && a[i] === b[j]) { ops.push([" ", a[i]]); i++; j++; }
    else if (j < m && (i >= n || lcs[i][j + 1] >= lcs[i + 1][j])) { ops.push(["+", b[j]]); j++; }
    else { ops.push(["-", a[i]]); i++; }
  }
  const keep = ops.map((op, k) => op[0] !== " " || ops.slice(Math.max(0, k - 3), k + 4).some(o => o[0] !== " "));
  const lines = [];
  let skipped = false;
  ops.forEach((op, k) => {
    if (!keep[k]) {
      if (!skipped) lines.push(h("span", { class: "gap" }, "  …"));
      skipped = true;
      return;
    }
    skipped = false;
    lines.push(h("span", { class: op[0] === "+" ? "add" : op[0] === "-" ? "del" : "" }, op[0] + " " + op[1]));
  });
  return h("pre", { class: "diff" }, lines);
}

// ---- markdown, as agents write it, built as elements: never as HTML

function markdown(text) {
  const frag = document.createDocumentFragment();
  const lines = text.replace(/\r\n?/g, "\n").split("\n");
  let i = 0;
  const isTableRule = l => /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?\s*$/.test(l);
  const cells = l => l.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map(c => c.trim());
  const listItem = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
  const blockStart = l => /^(\s*)(```|~~~|#{1,6}\s|>|([-*+]|\d{1,9}[.)])\s)/.test(l) || /^\s*([-*_])(\s*\1){2,}\s*$/.test(l);
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s*(```+|~~~+)\s*(\S*)/);
    if (fence) {
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(fence[1])) body.push(lines[i++]);
      i++;
      frag.append(h("pre", {}, h("code", {}, body.join("\n"))));
      continue;
    }
    if (!line.trim()) { i++; continue; }
    const heading = line.match(/^(#{1,6})\s+(.*?)\s*#*\s*$/);
    if (heading) {
      frag.append(h("h" + Math.min(5, heading[1].length + 2), {}, inline(heading[2])));
      i++;
      continue;
    }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { frag.append(h("hr", {})); i++; continue; }
    if (/^\s*>/.test(line)) {
      const body = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ""));
      frag.append(h("blockquote", {}, markdown(body.join("\n"))));
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && isTableRule(lines[i + 1])) {
      const head = cells(line);
      i += 2;
      const body = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) body.push(cells(lines[i++]));
      frag.append(h("div", { class: "table" }, h("table", {},
        h("thead", {}, h("tr", {}, head.map(c => h("th", {}, inline(c))))),
        h("tbody", {}, body.map(r => h("tr", {}, head.map((_, k) => h("td", {}, inline(r[k] || "")))))))));
      continue;
    }
    const item = line.match(listItem);
    if (item) {
      const indent = item[1].length;
      const ordered = /\d/.test(item[2]);
      const list = h(ordered ? "ol" : "ul", {});
      if (ordered && parseInt(item[2], 10) !== 1) list.setAttribute("start", parseInt(item[2], 10));
      while (i < lines.length) {
        const m = lines[i].match(listItem);
        if (!m || m[1].length !== indent || /\d/.test(m[2]) !== ordered) break;
        const body = [m[3]];
        i++;
        while (i < lines.length && lines[i].trim() && !(lines[i].match(listItem) && lines[i].match(listItem)[1].length <= indent)) {
          body.push(lines[i].slice(Math.min(lines[i].length - lines[i].trimStart().length, indent + 2)));
          i++;
        }
        // A blank line goes on with the list when an item follows it.
        while (i < lines.length && !lines[i].trim()) {
          const next = (lines[i + 1] || "").match(listItem);
          if (!next || next[1].length < indent) break;
          i++;
        }
        const [first, ...rest] = body;
        list.append(h("li", {}, inline(first), rest.length ? markdown(rest.join("\n")) : null));
      }
      frag.append(list);
      continue;
    }
    const para = [];
    while (i < lines.length && lines[i].trim() && !(para.length && blockStart(lines[i])) && !(lines[i].includes("|") && isTableRule(lines[i + 1] || ""))) para.push(lines[i++].trim());
    frag.append(h("p", {}, inline(para.join("\n"), true)));
  }
  return frag;
}

const INLINE = /(`+)([\s\S]*?[^`])\1(?!`)|\*\*(?=\S)([\s\S]*?\S)\*\*|__(?=\S)([\s\S]*?\S)__|\*(?=\S)([^*]*?\S)\*|(?<![\w])_(?=\S)([^_]*?\S)_(?![\w])|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[^\s<>()]+[^\s<>().,;:!?'"*_])/g;

function inline(text, breaks) {
  const out = [];
  let at = 0;
  const plain = s => {
    if (!breaks) { out.push(s); return; }
    s.split("\n").forEach((part, k) => { if (k) out.push(h("br", {})); out.push(part); });
  };
  for (const m of text.matchAll(INLINE)) {
    plain(text.slice(at, m.index));
    at = m.index + m[0].length;
    if (m[1]) out.push(h("code", {}, m[2].replace(/^ (.*) $/, "$1")));
    else if (m[3] || m[4]) out.push(h("strong", {}, inline(m[3] || m[4])));
    else if (m[5] || m[6]) out.push(h("em", {}, inline(m[5] || m[6])));
    else if (m[7]) out.push(h("a", { href: m[8], target: "_blank", rel: "noopener noreferrer" }, m[7]));
    else if (m[9]) out.push(h("a", { href: m[9], target: "_blank", rel: "noopener noreferrer" }, m[9]));
  }
  plain(text.slice(at));
  return out;
}

// ---- settings: this device, its notifications, and pairing others

function settingsView() {
  const el = h("div", { class: "page" });
  const pushPanel = h("section", { class: "panel" });
  const pairPanel = h("section", { class: "panel" });
  const devicesPanel = h("section", { class: "panel" });
  const label = h("input", { type: "text", placeholder: "phone", "aria-label": "The new device's name", maxlength: "64" });
  const invite = h("div", { class: "qr" });
  el.append(pushPanel, pairPanel, devicesPanel, h("p", { class: "empty" }, ""));
  let list = [];

  async function loadDevices() {
    try {
      list = (await api("GET", "/api/devices")).devices;
    } catch { /* drawn as empty */ }
    drawDevices();
  }

  function drawDevices() {
    put(devicesPanel, h("h2", {}, "Paired devices"), ...list.map(d =>
      h("div", { class: "device" },
        h("div", {}, h("b", {}, d.label + (d.this ? " (this device)" : "")),
          h("span", {}, "paired " + new Date(d.created).toLocaleDateString() + (d.push ? " · notifications on" : "") + (d.expires ? " · until " + new Date(d.expires).toLocaleDateString() : ""))),
        h("button", {
          class: "small-button", type: "button",
          onclick: async () => {
            if (!confirm(d.this ? "Unpair this device? It needs a new code to come back." : "Unpair " + d.label + "? It loses access at once.")) return;
            try {
              await api("DELETE", "/api/devices/" + encodeURIComponent(d.id));
              if (d.this) unpaired("This device is unpaired.");
              else loadDevices();
            } catch (e) {
              toast(e.message, e.next, true);
            }
          },
        }, "Unpair"))));
  }

  async function drawPush() {
    const status = await pushStatus();
    const button = h("button", { class: "button" + (status.on ? "" : " primary"), type: "button", disabled: !status.can },
      status.on ? "Turn notifications off" : "Turn notifications on");
    button.addEventListener("click", async () => {
      button.disabled = true;
      try {
        if (status.on) await pushOff();
        else await pushOn();
      } catch (e) {
        toast(e.message || String(e), e.next, true);
      }
      drawPush();
      loadDevices();
    });
    put(pushPanel, h("h2", {}, "Notifications"),
      h("p", { class: "muted" }, status.why || (status.on
        ? "This device is told when an agent needs you or finishes, unless it has tmag on screen or you are at a terminal that shows the agent."
        : "Be told when an agent needs you or finishes, while you are away.")),
      button);
  }

  pairPanel.append(h("h2", {}, "Pair a device"),
    h("p", { class: "muted" }, "A code pairs one device within ten minutes. Scan it with the device's camera, or enter the code on its tmag page."),
    h("form", {
      class: "field",
      onsubmit: async e => {
        e.preventDefault();
        try {
          const inv = await api("POST", "/api/devices", { label: label.value.trim() });
          const parts = [];
          if (inv.qr) parts.push(h("img", { src: "data:image/svg+xml;charset=utf-8," + encodeURIComponent(inv.qr), alt: "QR code of " + inv.url }));
          parts.push(h("span", { class: "code" }, inv.code), h("span", { class: "muted" }, "until " + new Date(inv.expires).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })));
          if (IOS || /iPhone|iPad/.test(label.value)) parts.push(h("span", { class: "muted" }, "On an iPhone: open it, add it to the Home Screen, and enter the code there."));
          put(invite, ...parts);
        } catch (err) {
          toast(err.message, err.next, true);
        }
      },
    }, label, h("button", { class: "button primary", type: "submit" }, "Get a code")),
    invite);

  return {
    el,
    update() {
      drawBar("Settings", "", true);
      document.title = "Settings · tmag";
      if (!this.drawn) {
        this.drawn = true;
        drawPush();
        loadDevices();
      }
      el.lastChild.textContent = state.version ? "tmag-client " + state.version : "";
    },
  };
}

// ---- notifications

// registration is the page's service worker, which shows notifications.
async function registration() {
  const reg = await Promise.race([navigator.serviceWorker.ready, new Promise(r => setTimeout(r, 5000))]);
  if (!reg) throw new Error("The page's service worker did not start; reload the page.");
  return reg;
}

function bytes(b64) {
  const s = atob(b64.replace(/-/g, "+").replace(/_/g, "/").padEnd(Math.ceil(b64.length / 4) * 4, "="));
  return Uint8Array.from(s, c => c.charCodeAt(0));
}

async function pushInfo() {
  state.push = await api("GET", "/api/push");
  return state.push;
}

// pushStatus says whether this device can turn notifications on, whether
// they are on, and why not when not.
async function pushStatus() {
  if (!isSecureContext || !("serviceWorker" in navigator) || !("PushManager" in window) || !("Notification" in window)) {
    if (IOS && !STANDALONE) return { can: false, why: "On iPhone and iPad, notifications come to tmag added to the Home Screen: Share, then Add to Home Screen, and pair from there." };
    if (!isSecureContext) return { can: false, why: "Notifications need the page over https, through the gateway's tunnel." };
    return { can: false, why: "This browser has no Web Push." };
  }
  let info;
  try { info = await pushInfo(); } catch (e) { return { can: false, why: e.message }; }
  if (info.reason === "off") return { can: false, why: "The gateway pushes nothing: it runs with --push \"\"." };
  if (info.reason === "no_public") return { can: false, why: "The gateway pushes only when a tunnel reaches it: start it with --public and the tunnel's https address." };
  if (Notification.permission === "denied") return { can: false, why: "Notifications are blocked for this page in the browser's settings." };
  const reg = await registration();
  const sub = await reg.pushManager.getSubscription();
  return { can: true, on: !!sub && info.subscribed };
}

async function pushOn() {
  if (await Notification.requestPermission() !== "granted") throw new Error("Notifications were not allowed.");
  const info = state.push || await pushInfo();
  const reg = await registration();
  let sub = await reg.pushManager.getSubscription();
  const key = bytes(info.key);
  if (sub && sub.options && sub.options.applicationServerKey) {
    const had = new Uint8Array(sub.options.applicationServerKey);
    if (had.length !== key.length || had.some((x, k) => x !== key[k])) {
      await sub.unsubscribe();
      sub = null;
    }
  }
  sub = sub || await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
  await api("PUT", "/api/push", sub.toJSON());
  toast("Notifications are on");
}

async function pushOff() {
  const reg = await registration();
  const sub = await reg.pushManager.getSubscription();
  if (sub) await sub.unsubscribe();
  await api("DELETE", "/api/push");
  toast("Notifications are off");
}

// keepSubscription hands the gateway this browser's subscription again, which
// a browser may have renewed since.
async function keepSubscription() {
  try {
    if (!isSecureContext || !("serviceWorker" in navigator) || !("PushManager" in window) || Notification.permission !== "granted") return;
    const info = await pushInfo();
    if (!info.key || !info.subscribed) return;
    const reg = await registration();
    const sub = await reg.pushManager.getSubscription();
    if (sub) await api("PUT", "/api/push", sub.toJSON());
  } catch { /* the settings say why */ }
}

// ---- pairing

function deviceName() {
  const ua = navigator.userAgent;
  const kind = /iPad/.test(ua) || (IOS && !/iPhone/.test(ua)) ? "iPad" : /iPhone/.test(ua) ? "iPhone" : /Android/.test(ua) ? "Android" : /Mac/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "device";
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "";
  return kind + (STANDALONE ? " · Home Screen" : browser ? " · " + browser : "");
}

function pairView(r) {
  const el = h("div", { class: "page pair" });
  const input = h("input", { class: "code-input", type: "text", inputmode: "text", autocomplete: "one-time-code", autocapitalize: "characters", spellcheck: "false", placeholder: "XXXX-XXXX-XXXX", "aria-label": "Pairing code", value: r.code || "" });
  const result = h("div", {});
  let busy = false;
  let anyway = false;

  async function pair(code) {
    if (busy || !code.trim()) return;
    busy = true;
    put(result, h("p", { class: "muted" }, "Pairing…"));
    try {
      const resp = await fetch("/api/pair", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ code, label: deviceName() }) });
      const doc = await resp.json().catch(() => ({}));
      if (!resp.ok) throw new Failure((doc.error || {}).code, (doc.error || {}).message || "Pairing failed.");
      state.token = doc.token;
      state.notice = "";
      localStorage.setItem(TOKEN_KEY, doc.token);
      history.replaceState(null, "", "/#/");
      connect();
      present();
      show();
      toast("Paired as " + doc.device.label);
    } catch (e) {
      put(result, failureNode(e));
    } finally {
      busy = false;
    }
  }

  function draw() {
    const homeScreenFirst = IOS && !STANDALONE && !anyway;
    const parts = [h("h1", {}, "Pair this device")];
    if (state.notice) parts.push(h("p", { class: "error" }, state.notice));
    if (homeScreenFirst) {
      parts.push(
        h("p", { class: "lead" }, "On iPhone and iPad, tmag lives on the Home Screen: that is where it stays paired, and where notifications arrive."),
        h("section", { class: "panel" }, h("ol", {},
          h("li", {}, "Tap Share, then Add to Home Screen."),
          h("li", {}, "Open tmag from the Home Screen."),
          h("li", {}, r.code ? "Enter this code there:" : "Enter the pairing code there."))),
        r.code ? h("p", { class: "qr" }, h("span", { class: "code" }, r.code)) : null,
        h("button", { class: "link", type: "button", onclick: () => { anyway = true; draw(); } }, "Pair this browser instead"));
    } else {
      parts.push(
        h("p", { class: "lead" }, "Enter a pairing code. Get one in the gateway's tmux window with tmag-client auth pair, or under Settings on a device already paired."),
        h("form", { class: "panel", onsubmit: e => { e.preventDefault(); pair(input.value); } },
          input, h("button", { class: "button primary", type: "submit" }, "Pair")),
        result);
    }
    put(el, parts);
  }

  draw();
  if (r.code && !(IOS && !STANDALONE)) pair(r.code);
  return {
    el,
    update() {
      drawBar("tmag", "");
      document.title = "Pair · tmag";
    },
  };
}

function unpaired(why) {
  state.token = "";
  localStorage.removeItem(TOKEN_KEY);
  state.notice = why;
  state.sessions.clear();
  state.machines.clear();
  state.live = false;
  if (stream) stream.ctl.abort();
  stream = null;
  clearTimeout(retryTimer);
  if (location.hash !== "#/") history.replaceState(null, "", "/#/");
  view = null;
  show();
}

// ---- toasts

const toasts = document.getElementById("toasts");

function toast(text, detail, bad, href) {
  const t = h(href ? "a" : "div", { class: "toast" + (bad ? " bad" : ""), href, role: bad ? "alert" : "status" },
    href ? h("b", {}, text) : text, detail ? h("small", {}, detail) : null);
  toasts.append(t);
  while (toasts.children.length > 3) toasts.firstChild.remove();
  setTimeout(() => t.remove(), href ? 7000 : bad ? 6000 : 3000);
  if (href) t.addEventListener("click", () => t.remove());
}

// alerted shows an alert in place, unless its session is on screen.
function alerted(n) {
  if (view && view.name === "session" && view.key === n.key && visible()) return;
  toast(n.title, n.body, false, "#/s/" + n.key);
}

// ---- start

function opened(url) {
  try {
    const u = new URL(url, location.origin);
    if (u.origin === location.origin && /^#\/(s\/.+)?$/.test(u.hash || "#/")) location.hash = u.hash || "#/";
  } catch { /* not a route */ }
}

function start() {
  if ("serviceWorker" in navigator && isSecureContext) {
    navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => {});
    navigator.serviceWorker.onmessage = e => { if (e.data && e.data.type === "open") opened(e.data.url); };
    navigator.serviceWorker.startMessages();
  }
  addEventListener("hashchange", show);
  show();
  if (state.token) {
    connect();
    present();
    keepSubscription();
  }
}

start();
