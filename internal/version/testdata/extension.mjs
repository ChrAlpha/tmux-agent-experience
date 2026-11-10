// Drives one of tmag's extensions for Pi or oh-my-pi (node extension.mjs HOST
// EXTENSION) through a scenario of that host's events, with the context its
// handlers see, as TestExtensionsReport runs it. The extension's reports go
// to the tmag its hook command finds.
import { pathToFileURL } from "node:url";

const [host, extension] = process.argv.slice(2);
const handlers = new Map();
const api = { on: (name, handler) => handlers.set(name, [...(handlers.get(name) ?? []), handler]) };
(await import(pathToFileURL(extension).href)).default(api);

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

async function emit(name, event = {}, extra) {
	for (const handler of handlers.get(name) ?? []) await handler({ type: name, ...event }, ctx(extra));
}
const user = (text) => ({ role: "user", content: [{ type: "text", text }], timestamp: Date.now() + entries.length });
const assistant = (stopReason) => ({ role: "assistant", content: [{ type: "text", text: "secret reply" }], stopReason });

append({ type: "message", message: user("an earlier prompt") });
append({ type: "message", message: assistant("stop") });
if (host === "pi") {
	await emit("session_start", { reason: "resume" });
	// A prompt: Pi records the user message once the handlers return.
	idle = false;
	await emit("before_agent_start", { messageType: "user", prompt: "secret prompt" });
	await emit("agent_start");
	await emit("message_end", { message: { role: "system", content: "" } });
	const prompt = user("secret prompt");
	await emit("message_end", { message: prompt });
	append({ type: "message", message: prompt });
	await emit("tool_execution_start", { toolCallId: "c1", toolName: "bash", args: { command: "secret" } });
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
	await emit("session_shutdown", { reason: "quit" });
} else {
	await emit("session_start");
	// Subagents run the extension too.
	await emit("tool_execution_start", { toolCallId: "x1", toolName: "bash" }, sub);
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
	await emit("tool_approval_resolved", { toolCallId: "c2", toolName: "bash", approved: true });
	await emit("tool_execution_end", { toolCallId: "c2", toolName: "bash" });
	append({ type: "message", message: assistant("stop") });
	await emit("agent_end", { willContinue: true });
	await emit("agent_end", { willContinue: true, awaitingAsyncWork: true });
	// /branch moves to a new session, and a contested session to another
	// without a word.
	id = "s2";
	await emit("session_branch", { reason: "branch" });
	id = "s3";
	await emit("before_agent_start", { reason: "revive" });
	await emit("agent_start");
	await emit("agent_end", {});
	await emit("session_shutdown");
}
