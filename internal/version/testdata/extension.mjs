// Drives one of tmag's extensions for Pi or oh-my-pi (node extension.mjs HOST
// EXTENSION) through a scenario of that host's events, with the context its
// handlers see, as TestExtensionsReport runs it. The extension's reports go
// to the tmag its hook command finds. oh-my-pi also gets a second API, as a
// subagent does, to check that only the main instance owns the inbox.
import { spawn } from "node:child_process";
import { appendFileSync, existsSync, mkdirSync, unlinkSync } from "node:fs";
import { connect } from "node:net";
import { pathToFileURL } from "node:url";

const [host, extension] = process.argv.slice(2);
const uid = process.getuid?.();
const base = uid === undefined ? "" : `/tmp/tmag-${uid}`;
const dir = uid === undefined ? "" : `${base}/inbox`;
const inbox = uid === undefined ? "" : `${dir}/${host}-${process.pid}.sock`;
const stale = uid === undefined ? "" : `${dir}/${host}-999999.sock`;
if (uid !== undefined) {
	for (const path of [base, dir]) {
		try {
			mkdirSync(path, { mode: 0o700 });
		} catch (err) {
			if (err.code !== "EEXIST") throw err;
		}
	}
	try {
		unlinkSync(stale);
	} catch {}
	// A refusing socket younger than a minute must survive the inbox's sweep.
	// A child that is killed leaves its socket path behind, refusing.
	const child = spawn(process.execPath, ["-e", 'const { createServer } = require("node:net"); createServer().listen(process.env.TMAG_STALE, () => {}); setInterval(() => {}, 1000);'], {
		env: { ...process.env, TMAG_STALE: stale },
		stdio: "ignore",
	});
	for (let n = 0; n < 200 && !existsSync(stale); n++) await new Promise((resolve) => setTimeout(resolve, 10));
	if (!existsSync(stale)) {
		child.kill("SIGKILL");
		throw new Error(`no stale socket at ${stale}`);
	}
	const exited = new Promise((resolve) => {
		if (child.exitCode !== null || child.signalCode !== null) resolve();
		else child.once("exit", resolve);
	});
	child.kill("SIGKILL");
	await exited;
}

const calls = process.env.TMAG_CALLS ?? "";
const makeApi = (name) => {
	const handlers = new Map();
	return {
		name,
		handlers,
		on: (event, handler) => handlers.set(event, [...(handlers.get(event) ?? []), handler]),
		sendUserMessage: (content, options) => {
			if (calls) appendFileSync(calls, JSON.stringify({ api: name, text: String(content), ...(options ? { options } : {}) }) + "\n");
			return Promise.resolve();
		},
	};
};
const main = makeApi("main");
const child = host === "omp" ? makeApi("child") : undefined;
const mod = await import(pathToFileURL(extension).href);
mod.default(main);
if (child) mod.default(child);

// A session as the host keeps it: entries up a branch from the leaf.
let id = "s1";
let idle = true;
const entries = [];
const append = (entry) => entries.push({ id: `e${entries.length}`, parentId: entries.at(-1)?.id ?? null, ...entry });
const sessionManager = {
	getSessionId: () => id,
	getSessionFile: () => `/sessions/${id}.jsonl`,
	getLeafEntry: () => entries.at(-1),
	getEntry: (entryId) => entries.find((e) => e.id === entryId),
};
const ctx = (extra = {}) => ({ cwd: "/work", mode: "tui", isIdle: () => idle, sessionManager, ...extra });
const sub = { agent: { kind: "sub" } };

async function emit(name, event = {}, extra, api = main) {
	for (const handler of api.handlers.get(name) ?? []) await handler({ type: name, ...event }, ctx(extra));
}

/** Waits for the main instance's inbox to listen. */
async function waitInbox() {
	for (let n = 0; n < 200 && !existsSync(inbox); n++) await new Promise((resolve) => setTimeout(resolve, 10));
	if (!existsSync(inbox)) throw new Error(`no inbox at ${inbox}`);
}

/** Reads one answer line from an open inbox connection. */
function readAnswer(conn) {
	let buffer = "";
	return new Promise((resolve, reject) => {
		const timer = setTimeout(() => reject(new Error("no inbox answer")), 2_000);
		conn.setEncoding("utf8");
		conn.on("data", (chunk) => {
			buffer += chunk;
			const at = buffer.indexOf("\n");
			if (at >= 0) {
				clearTimeout(timer);
				resolve(buffer.slice(0, at));
			}
		});
		conn.on("error", reject);
	});
}

/** Sends one message for session and waits for its answer. */
async function deliver(text, session = id) {
	const conn = connect(inbox);
	await new Promise((resolve, reject) => {
		conn.once("connect", resolve);
		conn.once("error", reject);
	});
	const answer = readAnswer(conn);
	conn.write(JSON.stringify({ send: text, session }) + "\n");
	const reply = await answer;
	conn.end();
	const parsed = JSON.parse(reply);
	if (!parsed.ok) throw new Error(`send refused: ${parsed.error}`);
}

/** The inbox close unlinks its socket. */
function closed() {
	if (existsSync(inbox)) throw new Error(`inbox not unlinked: ${inbox}`);
}

/** An incomplete request is destroyed by close, not left to its timer. */
async function stalledAtClose() {
	const stalled = connect(inbox);
	stalled.on("error", () => {});
	await new Promise((resolve, reject) => {
		stalled.once("connect", resolve);
		stalled.once("error", reject);
	});
	stalled.unref?.();
	const gone = new Promise((resolve) => stalled.once("close", resolve));
	stalled.write('{"send":"incomplete"');
	await emit("session_shutdown", host === "pi" ? { reason: "quit" } : {});
	closed();
	await Promise.race([gone, new Promise((_, reject) => setTimeout(() => reject(new Error("a stalled inbox connection survived close")), 1_000))]);
}

const user = (text) => ({ role: "user", content: [{ type: "text", text }], timestamp: Date.now() + entries.length });
const assistant = (stopReason) => ({ role: "assistant", content: [{ type: "text", text: "secret reply" }], stopReason });

append({ type: "message", message: user("an earlier prompt") });
append({ type: "message", message: assistant("stop") });
if (host === "pi") {
	await emit("session_start", { reason: "resume" });
	await waitInbox();
	if (uid !== undefined && !existsSync(stale)) throw new Error("a fresh refusing socket was removed");
	await deliver("send-idle");
	// A prompt: Pi records the user message once the handlers return.
	idle = false;
	await emit("before_agent_start", { messageType: "user", prompt: "secret prompt" });
	await emit("agent_start");
	await emit("message_end", { message: { role: "system", content: "" } });
	const prompt = user("secret prompt");
	await emit("message_end", { message: prompt });
	append({ type: "message", message: prompt });
	await emit("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "secret" } });
	await deliver("send-running");
	await emit("tool_execution_start", { toolCallId: "c2", toolName: "read", parentToolCallId: "c1" });
	await emit("tool_execution_end", { toolCallId: "c2", toolName: "read", parentToolCallId: "c1" });
	await emit("ui_prompt_start", { kind: "confirm", title: "secret" });
	await emit("ui_prompt_start", { kind: "select" }); // nested in the first
	await emit("ui_prompt_end");
	await emit("tool_execution_end", { toolCallId: "c1", toolName: "bash" });
	append({ type: "message", message: assistant("stop") });
	await emit("agent_settled", { aborted: false });
	// A run an extension's message starts.
	await emit("before_agent_start", { messageType: "custom" });
	await emit("agent_start");
	await emit("agent_settled", { aborted: true });
	// A prompt that no user message carries, then a failed response.
	await emit("before_agent_start", { messageType: "user" });
	await emit("agent_start");
	append({ type: "message", message: assistant("error") });
	await emit("message_end", { message: assistant("error") });
	await emit("agent_settled", { aborted: false });
	// A dialog the person opens while Pi is idle.
	idle = true;
	await emit("ui_prompt_start", { kind: "select" });
	await emit("ui_prompt_end");
	await stalledAtClose();
	try {
		unlinkSync(stale);
	} catch {}
} else {
	await emit("session_start");
	await waitInbox();
	if (uid !== undefined && !existsSync(stale)) throw new Error("a fresh refusing socket was removed");
	await deliver("send-idle");
	// Subagents run the extension too: a second API is bound to the child's
	// context, and its instance must not take the main inbox.
	await emit("tool_execution_start", { toolCallId: "x1", toolName: "bash" }, sub, child);
	// A prompt: oh-my-pi records the user message before reporting it.
	idle = false;
	await emit("before_agent_start", { reason: "prompt", prompt: "secret prompt" });
	await emit("agent_start");
	const prompt = user("secret prompt");
	append({ type: "message", message: prompt });
	await emit("message_end", { message: { ...prompt } });
	await emit("tool_execution_start", { toolCallId: "c1", toolName: "ask" });
	await emit("tool_execution_end", { toolCallId: "c1", toolName: "ask" });
	await emit("tool_execution_start", { toolCallId: "c2", toolName: "bash" });
	await emit("tool_approval_requested", { toolCallId: "c2", toolName: "bash" });
	await deliver("send-running");
	await emit("tool_approval_resolved", { toolCallId: "c2", toolName: "bash", approved: true });
	await emit("tool_execution_end", { toolCallId: "c2", toolName: "bash" });
	append({ type: "message", message: assistant("stop") });
	await emit("agent_end", { willContinue: true });
	await emit("agent_end", { willContinue: true, awaitingAsyncWork: true });
	// A send opened for s1 is refused once the manager serves s2, even
	// before the switch event: the connection is already open, and the
	// request completes after the manager changed.
	const pending = connect(inbox);
	await new Promise((resolve, reject) => {
		pending.once("connect", resolve);
		pending.once("error", reject);
	});
	const partial = JSON.stringify({ send: "old session", session: id });
	pending.write(partial.slice(0, -1));
	id = "s2";
	const answer = readAnswer(pending);
	pending.write("}\n");
	const refused = JSON.parse(await answer);
	if (refused.ok !== false || refused.refused !== "changed") throw new Error(`an old session was not refused: ${JSON.stringify(refused)}`);
	pending.end();
	// /branch moves to a new session, and a contested session to another
	// without a word.
	await emit("session_branch", { reason: "branch" });
	id = "s3";
	await emit("before_agent_start", { reason: "revive" });
	await emit("agent_start");
	await emit("agent_end", {});
	await stalledAtClose();
}
try {
	unlinkSync(stale);
} catch {}
