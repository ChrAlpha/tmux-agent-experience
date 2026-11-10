// tmag's sensor for oh-my-pi: reports the session's state to tmag, which shows
// it in tmux and lets other agents and clients see and prompt it. Runs `tmag
// hook omp` for each event that changes the state (see report.ts), under
// oh-my-pi's names for the events; tmag maps them in internal/adapter.
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { lastStop, lastTurn, type Payload, quietly, reporter, session, turnOf } from "./report.ts";

// binpath.HookCommand("omp"): the tmux plugin's tmag, else tmag on PATH.
const hook = "for r in \"${TMUX_PLUGIN_MANAGER_PATH%/}\" \"$HOME/.tmux/plugins\" \"${XDG_CONFIG_HOME:-$HOME/.config}/tmux/plugins\"; do case $r in /*) t=\"$r/tmux-agent-experience/bin/tmag\"; [ -x \"$t\" ] && exec \"$t\" hook omp 2>/dev/null;; esac; done; t=$(command -v tmag) && exec \"$t\" hook omp 2>/dev/null; exit 0";

type Context = Parameters<typeof session>[0] & { agent?: { kind: string } };

export default function (omp: ExtensionAPI) {
	const { report, drained } = reporter(hook);
	// What reports of the session last reported started carry. oh-my-pi
	// moves the process to another session with session_switch (/new,
	// /resume, /fork) or session_branch (/branch, /btw), and may move a
	// contested session to a file of its own: every report checks it, ending
	// the session left.
	let current: Payload | undefined;
	// A run a prompt of the user's starts is reported with the prompt's
	// message, which names its turn; any other run when it starts, and one
	// whose prompt turns out to be no user message, as a skill's, when the
	// run goes on.
	let prompted = false;
	// oh-my-pi runs this extension for its subagents too, whose work belongs
	// to the main session's turn.
	const main = (ctx: Context) => ctx.agent?.kind !== "sub";

	// enter reports that ctx's session started, as event says and for the
	// reason source, unless it is the session last reported; it returns what
	// every payload of the session carries.
	const enter = (ctx: Context, event = "session_switch", source = "switch"): Payload => {
		const reported = session(ctx);
		if (reported.session_id === current?.session_id) return reported;
		if (current) report({ hook_event_name: "session_shutdown", ...current, reason: source });
		current = reported;
		prompted = false;
		report({ hook_event_name: event, ...reported, source, turn_id: lastTurn(ctx) });
		return reported;
	};
	const begin = (ctx: Context) => {
		const reported = enter(ctx);
		if (!prompted) return;
		prompted = false;
		report({ hook_event_name: "agent_start", ...reported });
	};

	omp.on(
		"session_start",
		quietly((_event, ctx) => {
			if (main(ctx)) enter(ctx, "session_start", "startup");
		}),
	);
	for (const name of ["session_switch", "session_branch"] as const) {
		omp.on(
			name,
			quietly((event, ctx) => {
				if (main(ctx)) enter(ctx, "session_switch", event.reason);
			}),
		);
	}
	omp.on(
		"session_shutdown",
		quietly(async (_event, ctx) => {
			if (!main(ctx)) return;
			report({ hook_event_name: "session_shutdown", ...enter(ctx), reason: "quit" });
			// oh-my-pi gives this event 2 s.
			await drained(1_500);
		}),
	);
	omp.on(
		"before_agent_start",
		quietly((event, ctx) => {
			if (!main(ctx)) return;
			enter(ctx);
			prompted = event.reason === "prompt";
		}),
	);
	omp.on(
		"agent_start",
		quietly((_event, ctx) => {
			if (!main(ctx)) return;
			const reported = enter(ctx);
			if (!prompted) report({ hook_event_name: "agent_start", ...reported });
		}),
	);
	omp.on(
		"message_end",
		quietly((event, ctx) => {
			if (!main(ctx)) return;
			if (event.message.role !== "user") {
				// A system prompt may go ahead of the user's.
				if (event.message.role !== "system") begin(ctx);
				return;
			}
			const reported = enter(ctx);
			prompted = false;
			const sent = event.message.timestamp;
			report(async () => ({ hook_event_name: "message_end", ...reported, turn_id: await turnOf(ctx, sent) }));
		}),
	);
	for (const name of ["tool_execution_start", "tool_execution_end", "tool_approval_requested", "tool_approval_resolved"] as const) {
		omp.on(
			name,
			quietly((event, ctx) => {
				if (!main(ctx)) return;
				begin(ctx);
				report({ hook_event_name: name, ...enter(ctx), tool_name: event.toolName, tool_use_id: event.toolCallId });
			}),
		);
	}
	omp.on(
		"agent_end",
		quietly((event, ctx) => {
			// An end oh-my-pi goes on from by itself is no end of the turn.
			// One that waits on background work is: a job's result starts a
			// run of its own, and oh-my-pi reports no end of the wait.
			if (!main(ctx) || (event.willContinue && !event.awaitingAsyncWork)) return;
			prompted = false;
			report({ hook_event_name: "agent_end", ...enter(ctx), stop_reason: lastStop(ctx) });
		}),
	);
}
