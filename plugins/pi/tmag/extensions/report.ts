// How tmag's extensions for Pi and oh-my-pi report to tmag: each event that
// changes a session runs `tmag hook`, one at a time and in the order of the
// events, without holding up the agent. A missing or failing tmag changes
// nothing in the agent. What goes to tmag are ids, paths and states, never a
// prompt, a message or a tool's input.
//
// Both extensions ship this file as it is: edit plugins/pi/tmag/extensions/
// report.ts and copy it to plugins/omp/tmag/extensions/.
import { spawn } from "node:child_process";

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
 * in: tmag treats a session no person types into as a service.
 */
export function session(ctx: Context): Payload {
	const file = ctx.sessionManager.getSessionFile();
	return {
		session_id: ctx.sessionManager.getSessionId(),
		cwd: ctx.cwd,
		...(file ? { transcript_path: file } : {}),
		...(ctx.mode ? { mode: ctx.mode } : {}),
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
