// How tmag's extensions for Pi and oh-my-pi report to tmag: each event that
// changes a session runs `tmag hook`, one at a time and in the order of the
// events, without holding up the agent. A missing or failing tmag changes
// nothing in the agent. What goes to tmag are ids, paths and states, never a
// prompt, a message or a tool's input.
//
// The inbox below serves what `tmag send` hands an agent: its text reaches
// the agent, and tmag's reports carry only where the socket is.
//
// Both extensions ship this file as it is: edit plugins/pi/tmag/extensions/
// report.ts and copy it to plugins/omp/tmag/extensions/.
import { spawn } from "node:child_process";
import { lstatSync, mkdirSync, readdirSync, unlinkSync } from "node:fs";
import type { Stats } from "node:fs";
import { connect, createServer } from "node:net";
import type { Socket } from "node:net";
import { join } from "node:path";

/** A hook payload, in the fields tmag reads (adapter.Payload). */
export type Payload = Record<string, unknown>;

/** The part of an agent's session an extension reads. */
interface Entry {
	type: string;
	id: string;
	parentId: string | null;
	message?: { role?: string; timestamp?: number; stopReason?: string };
}

interface Context {
	cwd: string;
	/** How the agent runs: "tui" for a person at its terminal. */
	mode?: string;
	/** Whether the agent is idle (not streaming). */
	isIdle(): boolean;
	sessionManager: {
		getSessionId(): string;
		getSessionFile(): string | undefined;
		getLeafEntry(): Entry | undefined;
		getEntry(id: string): Entry | undefined;
	};
}

/** How long one hook may run: tmag ends its own within 3 s (hook.Deadline). */
const hookLimit = 5_000;

/** How far back from the newest entry a turn's user message is looked for. */
const depth = 10_000;

/**
 * Reports go out one at a time through `command`, a shell command running
 * `tmag hook AGENT`. `report` queues a payload, or a function making one
 * when its turn comes; `drained` waits until the queue is done, at most ms.
 */
export function reporter(command: string) {
	let queue: Promise<void> = Promise.resolve();
	const report = (payload: Payload | (() => Promise<Payload>)) => {
		queue = queue
			.then(async () => run(command, JSON.stringify(typeof payload === "function" ? await payload() : payload)))
			.catch(() => {});
	};
	const drained = (ms: number) =>
		new Promise<void>((resolve) => {
			const timer = setTimeout(resolve, ms);
			void queue.then(() => {
				clearTimeout(timer);
				resolve();
			});
		});
	return { report, drained };
}

/** How long one send may take to deliver its line. */
const inboxLimit = 5_000;

/** The largest send line an inbox reads. */
const inboxMax = 1 << 20;

/** How old a refusing socket must be before a sweep removes it. */
const staleAfter = 60_000;

/** SessionChanged reports a request for a session the agent no longer serves. */
export class SessionChanged extends Error {
	readonly refused = "changed";
}

/**
 * Serves the session's inbox on a Unix socket, for prompts `tmag send`
 * hands the agent: /tmp/tmag-<uid>/inbox/<agent>-<pid>.sock. The socket is
 * the agent's own, as answer hooks keep theirs under /tmp; both directories
 * must be this user's alone, or no inbox is served and payloads carry none.
 *
 * A connection sends one JSON line, {"send":TEXT,"session":ID}; deliver
 * checks the session the agent serves now and hands the text over in one
 * step. A mismatch throws SessionChanged, which is answered as a `changed`
 * refusal and delivers nothing. Nothing here throws into the agent, and
 * close destroys open connections, clears their timers and unlinks the
 * socket. Stale sockets of this agent that died are removed when a
 * connection to them is refused.
 */
export function inbox(agent: string, deliver: (text: string, session: string) => Promise<void> | void): { path: string | undefined; close(): void } {
	const uid = process.getuid?.();
	if (uid === undefined) return { path: undefined, close: () => {} };
	const base = join("/tmp", `tmag-${uid}`);
	const dir = join(base, "inbox");
	const socketPath = join(dir, `${agent}-${process.pid}.sock`);
	try {
		if (!makePrivate(base, uid) || !makePrivate(dir, uid)) return { path: undefined, close: () => {} };
	} catch {
		return { path: undefined, close: () => {} };
	}
	removeSocket(socketPath);
	sweep(dir, agent, socketPath);
	let closed = false;
	const clients = new Set<InboxClient>();
	const server = createServer((conn) => {
		if (closed) {
			conn.destroy();
			return;
		}
		conn.unref?.();
		const client: InboxClient = { conn };
		clients.add(client);
		serveInbox(client, deliver, () => clients.delete(client));
	});
	server.on("error", () => {});
	let listening = false;
	try {
		server.listen(socketPath, () => {
			listening = true;
		});
	} catch {
		return { path: undefined, close: () => {} };
	}
	server.unref();
	const close = () => {
		if (closed) return;
		closed = true;
		try {
			server.close();
		} catch {}
		for (const client of [...clients]) {
			if (client.timer) clearTimeout(client.timer);
			client.conn.destroy();
		}
		clients.clear();
		removeSocket(socketPath);
	};
	const socketReady = () => {
		try {
			return lstatSync(socketPath).isSocket();
		} catch {
			return false;
		}
	};
	return {
		get path() {
			return !closed && (listening || server.listening || socketReady()) ? socketPath : undefined;
		},
		close,
	};
}

/** One accepted inbox connection and its line timer. */
interface InboxClient {
	conn: Socket;
	timer?: ReturnType<typeof setTimeout>;
}

/** Creates path with mode 0700 unless it exists; false unless it is a directory of uid's alone. */
function makePrivate(path: string, uid: number): boolean {
	try {
		mkdirSync(path, { mode: 0o700 });
	} catch (err) {
		if ((err as NodeJS.ErrnoException)?.code !== "EEXIST") return false;
	}
	try {
		const st = lstatSync(path);
		return st.isDirectory() && st.uid === uid && (st.mode & 0o077) === 0;
	} catch {
		return false;
	}
}

/** Removes path if a socket is there. */
function removeSocket(path: string): void {
	try {
		if (lstatSync(path).isSocket()) unlinkSync(path);
	} catch {}
}

/** Probes the agent's other sockets; nothing is removed while it is served. */
function sweep(dir: string, agent: string, own: string): void {
	let names: string[];
	try {
		names = readdirSync(dir);
	} catch {
		return;
	}
	for (const name of names) {
		if (!name.startsWith(`${agent}-`) || !name.endsWith(".sock")) continue;
		const path = join(dir, name);
		if (path === own) continue;
		let st: Stats;
		try {
			st = lstatSync(path);
		} catch {
			continue;
		}
		if (!st.isSocket() || Date.now() - st.mtimeMs < staleAfter) continue;
		probe(path, st);
	}
}

/** Removes a socket whose connection is refused, as a dead agent's is. */
function probe(path: string, st: Stats): void {
	let conn: Socket;
	try {
		conn = connect(path);
	} catch {
		return;
	}
	conn.once("connect", () => conn.destroy());
	conn.once("error", (err: Error) => {
		conn.destroy();
		if ((err as NodeJS.ErrnoException).code === "ECONNREFUSED") removeIfSame(path, st);
	});
	conn.setTimeout(200, () => conn.destroy());
}

/** Removes path if it is still the socket st describes, never one bound since. */
function removeIfSame(path: string, st: Stats): void {
	try {
		const now = lstatSync(path);
		if (now.isSocket() && now.dev === st.dev && now.ino === st.ino) unlinkSync(path);
	} catch {}
}

/** Serves one send connection: one line in, one answer line out. */
function serveInbox(
	client: InboxClient,
	deliver: (text: string, session: string) => Promise<void> | void,
	done: () => void,
): void {
	const conn = client.conn;
	let buffer = "";
	let open = true;
	const finish = (reply: string) => {
		if (!open) return;
		open = false;
		if (client.timer) clearTimeout(client.timer);
		try {
			conn.end(reply);
		} catch {}
	};
	client.timer = setTimeout(() => {
		finish("");
		conn.destroy();
	}, inboxLimit);
	client.timer.unref?.();
	conn.on("error", () => {});
	conn.on("close", () => {
		if (client.timer) clearTimeout(client.timer);
		done();
	});
	conn.setEncoding("utf8");
	conn.on("data", (chunk: string) => {
		if (!open) return;
		buffer += chunk;
		const at = buffer.indexOf("\n");
		if (at < 0) {
			if (buffer.length > inboxMax) {
				finish("");
				conn.destroy();
			}
			return;
		}
		if (at > inboxMax) {
			finish("");
			conn.destroy();
			return;
		}
		conn.pause();
		let request: { send?: unknown; session?: unknown } | null;
		try {
			request = JSON.parse(buffer.slice(0, at));
		} catch {
			finish(JSON.stringify({ ok: false, error: "invalid request" }) + "\n");
			return;
		}
		if (!request || typeof request.send !== "string") {
			finish(JSON.stringify({ ok: false, error: "invalid request" }) + "\n");
			return;
		}
		const session = typeof request.session === "string" ? request.session : "";
		const finishDelivery = (err: unknown) => {
			if (err instanceof SessionChanged) {
				finish(JSON.stringify({ ok: false, refused: "changed", error: err.message }) + "\n");
				return;
			}
			finish(JSON.stringify({ ok: false, error: String(err) }) + "\n");
		};
		let result: Promise<void> | void;
		try {
			result = deliver(request.send as string, session);
		} catch (err) {
			finishDelivery(err);
			return;
		}
		Promise.resolve(result).then(
			() => finish(JSON.stringify({ ok: true }) + "\n"),
			(err) => finishDelivery(err),
		);
	});
}

/** Runs command with input on its standard input, as a child of the agent. */
function run(command: string, input: string): Promise<void> {
	return new Promise((resolve) => {
		const child = spawn("/bin/sh", ["-c", command], { stdio: ["pipe", "ignore", "ignore"] });
		const timer = setTimeout(() => child.kill("SIGKILL"), hookLimit);
		const done = () => {
			clearTimeout(timer);
			resolve();
		};
		child.on("error", done);
		child.on("close", done);
		child.stdin.on("error", () => {});
		child.stdin.end(input);
	});
}

/**
 * The session ctx serves, as every payload names it, and the mode it runs
 * in: tmag treats a session no person types into as a service. inbox is the
 * socket the extension serves for sends, when it could listen.
 */
export function session(ctx: Context, inbox?: string): Payload {
	const file = ctx.sessionManager.getSessionFile();
	return {
		session_id: ctx.sessionManager.getSessionId(),
		cwd: ctx.cwd,
		...(file ? { transcript_path: file } : {}),
		...(ctx.mode ? { mode: ctx.mode } : {}),
		...(inbox ? { inbox } : {}),
	};
}

/**
 * The newest entry, up the current branch, that is a user message (sent at
 * sent, when given): its id names the turn the message starts.
 */
function userEntry(ctx: Context, sent?: number): string {
	const sm = ctx.sessionManager;
	let entry = sm.getLeafEntry();
	for (let n = 0; entry && n < depth; n++) {
		if (entry.type === "message" && entry.message?.role === "user" && (sent === undefined || entry.message.timestamp === sent)) {
			return entry.id;
		}
		entry = entry.parentId ? sm.getEntry(entry.parentId) : undefined;
	}
	return "";
}

/** The turn the session last ran, "" before its first. */
export function lastTurn(ctx: Context): string {
	return userEntry(ctx);
}

/**
 * The turn a user message sent at sent starts. The agent records the
 * message in its session around the event reporting it, so this waits a
 * little for it; "" when it never appears.
 */
export async function turnOf(ctx: Context, sent: number | undefined): Promise<string> {
	for (const until = Date.now() + 1_000; ; ) {
		const id = sent === undefined ? "" : userEntry(ctx, sent);
		if (id || Date.now() >= until) return id;
		await new Promise((resolve) => setTimeout(resolve, 10));
	}
}

/** How the session's last model response ended ("stop", "error", ...). */
export function lastStop(ctx: Context): string {
	const sm = ctx.sessionManager;
	let entry = sm.getLeafEntry();
	for (let n = 0; entry && n < depth; n++) {
		if (entry.type === "message" && entry.message?.role === "assistant") return entry.message.stopReason ?? "";
		entry = entry.parentId ? sm.getEntry(entry.parentId) : undefined;
	}
	return "";
}

/**
 * Wraps an event handler so that nothing it throws or rejects reaches the
 * agent, and nothing it returns decides anything there.
 */
export function quietly<A extends unknown[]>(handler: (...args: A) => unknown) {
	return (...args: A): Promise<void> | undefined => {
		try {
			const result = handler(...args);
			if (result instanceof Promise) return result.then(
				() => {},
				() => {},
			);
		} catch {}
		return undefined;
	};
}
