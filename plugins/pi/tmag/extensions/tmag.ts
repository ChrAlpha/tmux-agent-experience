// tmag's sensor for Pi: reports the session's state to tmag, which shows it in
// tmux and lets other agents and clients see and prompt it. Runs `tmag hook
// pi` for each event that changes the state (see report.ts), under Pi's names
// for the events; tmag maps them in internal/adapter.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { lastStop, lastTurn, quietly, reporter, session, turnOf } from "./report.ts";

// binpath.HookCommand("pi"): the tmux plugin's tmag, else tmag on PATH.
const hook = "for r in \"${TMUX_PLUGIN_MANAGER_PATH%/}\" \"$HOME/.tmux/plugins\" \"${XDG_CONFIG_HOME:-$HOME/.config}/tmux/plugins\"; do case $r in /*) t=\"$r/tmux-agent-experience/bin/tmag\"; [ -x \"$t\" ] && exec \"$t\" hook pi 2>/dev/null;; esac; done; t=$(command -v tmag) && exec \"$t\" hook pi 2>/dev/null; exit 0";

export default function (pi: ExtensionAPI) {
	const { report, drained } = reporter(hook);
	// A run a prompt of the user's starts is reported with the prompt's
	// message, which names its turn; any other run when it starts, and one
	// whose prompt turns out to be no user message when the run goes on.
	let prompted = false;
	const begin = (ctx: Parameters<typeof session>[0]) => {
		if (!prompted) return;
		prompted = false;
		report({ hook_event_name: "agent_start", ...session(ctx) });
	};
	// An extension's dialog is open during a turn, waiting on the user.
	let asking = false;

	pi.on(
		"session_start",
		quietly((event, ctx) => {
			report({ hook_event_name: "session_start", ...session(ctx), source: event.reason, turn_id: lastTurn(ctx) });
		}),
	);
	pi.on(
		"session_shutdown",
		quietly(async (event, ctx) => {
			report({ hook_event_name: "session_shutdown", ...session(ctx), reason: event.reason });
			await drained(4_000);
		}),
	);
	pi.on(
		"before_agent_start",
		quietly((event) => {
			prompted = event.messageType === "user";
		}),
	);
	pi.on(
		"agent_start",
		quietly((_event, ctx) => {
			if (!prompted) report({ hook_event_name: "agent_start", ...session(ctx) });
		}),
	);
	pi.on(
		"message_end",
		quietly((event, ctx) => {
			if (event.message.role !== "user") {
				// A system prompt may go ahead of the user's.
				if (event.message.role !== "system") begin(ctx);
				return;
			}
			prompted = false;
			const reported = session(ctx);
			const sent = event.message.timestamp;
			report(async () => ({ hook_event_name: "message_end", ...reported, turn_id: await turnOf(ctx, sent) }));
		}),
	);
	for (const name of ["tool_execution_start", "tool_execution_end"] as const) {
		pi.on(
			name,
			quietly((event, ctx) => {
				// A call another tool makes runs within that tool's.
				if (event.parentToolCallId) return;
				begin(ctx);
				report({ hook_event_name: name, ...session(ctx), tool_name: event.toolName, tool_use_id: event.toolCallId });
			}),
		);
	}
	pi.on(
		"ui_prompt_start",
		quietly((_event, ctx) => {
			// Outside a turn, the person opened the dialog and is at it.
			if (asking || ctx.isIdle()) return;
			begin(ctx);
			asking = true;
			report({ hook_event_name: "ui_prompt_start", ...session(ctx) });
		}),
	);
	pi.on(
		"ui_prompt_end",
		quietly((_event, ctx) => {
			if (!asking) return;
			asking = false;
			report({ hook_event_name: "ui_prompt_end", ...session(ctx) });
		}),
	);
	pi.on(
		"agent_settled",
		quietly((event, ctx) => {
			prompted = asking = false;
			report({ hook_event_name: "agent_settled", ...session(ctx), stop_reason: event.aborted ? "aborted" : lastStop(ctx) });
		}),
	);
}
