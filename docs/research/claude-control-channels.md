# Claude Code control channels: facts for `tmag send` / `interrupt` / `answer`

Date: 2026-11-03. Claude Code 2.1.317 (Linux, WSL2), model Haiku. Docs fetched
the same day as Markdown (`<page>.md`). The pages carry no version stamp. The
CHANGELOG's top entry is 2.1.317.

- **[H]** https://code.claude.com/docs/en/hooks
- **[HG]** https://code.claude.com/docs/en/hooks-guide
- **[IM]** https://code.claude.com/docs/en/interactive-mode
- **[KB]** https://code.claude.com/docs/en/keybindings
- **[AV]** https://code.claude.com/docs/en/agent-view
- **[CD]** https://code.claude.com/docs/en/claude-directory
- **[SA]** https://code.claude.com/docs/en/sub-agents
- **[CSM]** https://code.claude.com/docs/en/cross-session-messaging
- **[CLI]** https://code.claude.com/docs/en/cli-reference
- **[CL]** https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md

**Verified** means observed live here. **Documented** means stated in the docs
but not observed. **Inferred** means neither. This report builds on
`docs/research/claude-session-model.md` (on branch `docs/v0.1-feasibility`) and
does not repeat it.

## Summary

| Q | Answer | Evidence | Confidence |
| --- | --- | --- | --- |
| L1 schema | Print `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` or `"behavior":"deny","message":"…"` and exit 0. Exit 2 is ignored. Empty stdout or plain text means no decision. | [H]; allow, and deny with a message, verified | High |
| L1 timeout | The default is 600 s. At the timeout the hook gets SIGTERM, its output is discarded, and the dialog stays up. | [H]; SIGTERM at exactly the configured 60 s, verified | High |
| L1 dialog timing | **The TUI dialog is on screen while the hook runs.** It does not wait for the hook. The hook and the person race, and the first answer wins. A hook decision dismisses the open dialog. | Verified (dialog captured 1.5 s after the hook started) | High |
| L1 person answers first | "Yes" in the TUI: the tool runs at once. **The hook is not killed**; it runs on until it exits or times out, and its later output is ignored. It does not leak into a later request. Esc or "No": the turn is interrupted and the running hook gets SIGTERM. | Verified (T2, T5, T7) | High (Yes and Esc); "No" option inferred |
| L2 coverage | Fires for Bash, Write, `AskUserQuestion`, `ExitPlanMode`, and for subagent prompts (with `agent_id`). It does not fire for a sandboxed command's network request. MCP tools are documented to fire. | Verified, except sandbox network and MCP (documented) | High |
| L2 AskUserQuestion | Answerable: `allow` with `updatedInput` = original `tool_input` + `answers: {"<question text>": "<label>"}`. | Verified | High |
| L2 ExitPlanMode | **A plain `allow` was ignored** (the dialog stayed), contrary to [HG]. `allow` with `updatedInput` echoing `tool_input` worked. | Verified once each | Medium-high |
| L3 several hooks | All matching handlers start in parallel. The first decision applies at once, without waiting for silent hooks. A later conflicting decision is ignored. Identical handlers are deduped across settings files but not between a plugin and settings. | Verified (parallel start, first wins); dedupe documented | High |
| L4 interrupt | Esc and Ctrl+C both interrupt. **No hook fires** (no `Stop`, `StopFailure` or `PostToolUseFailure`). `idle_prompt` followed 39 s later. | Verified on 2.1.317 | High |
| L4 Esc side effects | One Esc on an empty idle composer: no visible effect. Esc Esc on an empty composer opens the rewind menu. Esc on a dialog declines it and ends the turn. | Verified | High |
| L5 transcript | `transcript_path` is in every payload we logged. The file is appended per entry, asynchronously, 0.3–2 s behind hooks. The turn id field is **`promptId`** on `type:"user"` entries, equal to the hooks' `prompt_id`. Subagent transcripts are at `<session>/subagents/agent-<agent_id>.jsonl` plus a `.meta.json`. | Verified | High |
| L6 background | Hooks, including a blocking `PermissionRequest`, run in `--bg` workers. **The hook answered a prompt no terminal showed.** Meanwhile `claude agents --json` showed `waiting` / `permission prompt` / `blocked`; `claude logs` and `claude attach` showed the normal dialog. | Verified | High |
| L7 composer | **A draft and the paste are concatenated into one prompt.** No single key clears the composer safely. Ctrl+U deletes only backward from the cursor, one line at a time, and is a no-op on an empty composer. Ctrl+C and Esc Esc clear everything but have side effects. | Verified | High |

## L1. `PermissionRequest` decisions and timing

### Documented [H]

- **Output.** `hookSpecificOutput.decision` takes these fields:
  - `behavior`: `allow` or `deny`;
  - `updatedInput`: allow only; replaces the whole input, and is re-checked against deny and ask rules;
  - `updatedPermissions`: allow only; `addRules`, `setMode`, … with destination `session`, `localSettings`, `projectSettings` or `userSettings`;
  - `message`: deny only, told to Claude;
  - `interrupt`: deny only, stops Claude.

  Deny and ask rules still apply over a hook's `allow`.
- **Exit codes.**
  - Exit 0 with valid JSON: the decision applies.
  - Exit 2 "isn't honored for this event and the permission flow proceeds unchanged"; stderr is discarded.
  - Any other code with JSON that passes validation: Claude ignores the exit code and applies the JSON.
  - Plain text or nothing on stdout: success with no decision.
  - Invalid JSON: a non-blocking error.
- **`onFailure: "block"` turns any hook failure into a deny on `PermissionRequest`.** A fail-open design must leave it unset.
- **`updatedInput` is validated like Claude's own input** (2.1.31x, [CL]). Unknown keys are dropped, and **input the tool rejects denies the call**. A malformed answer therefore denies; it does not fall back to the dialog.
- **Invalid output in a background session.** The agent-view row shows `hook output invalid: …`, and the session keeps waiting on the same request [AV].
- **Timeout.** The `timeout` field, default 600 s for `command` hooks. At the timeout Claude cancels the hook and discards its output, so it renders no decision [H, "Timeouts"].
- **Background process.** A decision printed by a background process after the hook exits is not read [H].

### Live setup

- Private tmux server (`-L research`, scratch `TMUX_TMPDIR`) and a scratch project under `/tmp/tmag-claude-research`.
- Claude started with `claude --plugin-dir <scratch plugin> --setting-sources local --permission-mode default --model haiku`.
- The scratch plugin registered three `PermissionRequest` handlers:
  1. a log-only probe that sleeps 3 s;
  2. blocking hook **A**, which waits for a per-pid control file and prints its contents;
  3. hook **B**, silent like tmag's own unless enabled.
- A log-only probe on every other event records event, tool, `agent_id`, `prompt_id`, the transcript's line count, timestamps and pids.

### Observations (verified)

- **T1: dialog concurrency.** Hook A started 40 ms after `PreToolUse`.
  - 1.5 s later, with A still waiting, the pane already showed the full Bash dialog ("Do you want to proceed? 1. Yes … 4. No").
  - `Notification` `permission_prompt` fired at +6 s with the hook still pending.
  - When A printed `allow` (+39 s), the dialog closed within ~0.1 s, the tool ran, and the transcript showed `Allowed by PermissionRequest hook`.
- **T3: deny with a message.**
  - Claude received `Error: <message>` as the tool result, and the UI showed `Denied by PermissionRequest hook`.
  - Applied 25 ms after the print, while the silent probe was still sleeping.
  - **A hook-denied call fires no `PostToolUseFailure`**; only `PostToolBatch`, then `Stop`.
- **T5: timeout.**
  - Hook timeout 60 s, no decision written.
  - A received SIGTERM at exactly +60.0 s.
  - The dialog stayed on screen unchanged, and no notice appeared in the transcript.
- **T2: person answers "Yes" while the hook waits.**
  - `PostToolUse` 30 ms after the keypress.
  - Hook A was **not signalled** and kept running.
  - When it later printed a `deny` (rc 0), nothing happened: no message, no double answer, no transcript record.
- **T7: decline with Esc while the hook waits.**
  - The turn ended (`Interrupted · What should Claude do instead?`).
  - Hook A got **SIGTERM** within ~0.5 s, and the 3 s probe never logged its end.
- **T7: no leak into a later request.**
  - Request 2 was answered "Yes" in the TUI, which orphaned its hook.
  - The orphan's later `deny` had no effect on request 3, whose dialog stayed open; request 3's own hook then decided it.
- **Transcript.** Hook decisions appear as an `attachment` of type `hook_permission_decision`, with keys `decision`, `hookEvent`, `toolUseID` and `type`. On the tool-result entry, `permissionDecision` is `{decision:"accept"|"reject", source:"hook", reasonType:"hook", reasonCode:…}`. Ignored late outputs left no record.

## L2. Coverage

| Prompt | `PermissionRequest`? | Answerable by hook | Evidence |
| --- | --- | --- | --- |
| Bash | yes | allow / deny | verified |
| Write (in a subagent) | yes, with `agent_id`, `agent_type` | allow | verified |
| Edit, WebFetch, other built-ins | "Matches on tool name, same values as PreToolUse" | allow / deny | documented [H]; Edit inferred like Write |
| MCP tool | yes, payload adds `mcp_server` | allow / deny | documented [H] |
| `AskUserQuestion` | yes; the question dialog shows meanwhile | `allow` + `updatedInput` with `answers` | verified |
| `ExitPlanMode` | yes; the plan dialog shows meanwhile | `allow` + `updatedInput` (echo `plan`, `planFilePath`) | verified; plain `allow` ignored |
| Subagent prompts | yes; `agent_id` present | as for the main agent | verified (background subagent) |
| Sandboxed command's network request | **no**; only `Notification` `permission_prompt` | none by hook | documented [H] |
| MCP elicitation | no; separate `Elicitation` event (`action`, `content`) | via `Elicitation` | documented [H] |

- **`AskUserQuestion`** ([H] AskUserQuestion input; verified).
  - Output used:

    ```json
    {"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedInput":{"questions":[…original…],"answers":{"Pick a color":"blue"}}}}}
    ```

  - Result: `User answered Claude's questions: · Pick a color → blue` plus `Allowed by PermissionRequest hook`, with "blue" chosen over the highlighted "red".
  - Multi-select answers join labels with commas [H].
  - Answers are keyed by **question text**, so the answering side needs the questions.
- **`ExitPlanMode`.**
  - The payload `tool_input` has keys `plan` and `planFilePath`.
  - A plain `allow` (the [HG] example) left the plan dialog up. No `hook_permission_decision` was recorded, and the hook exited 0.
  - The same session's next call with `allow` + `updatedInput: <tool_input>` approved.
  - Approval restored the pre-plan mode (`acceptEdits` here), as [HG] says. The hook path cannot "clear context" [HG].
- **Payloads** (38 captured).
  - Never `tool_use_id`.
  - Always `prompt_id`, `transcript_path`, `permission_mode`, `effort`, `cwd`, `session_id`.
  - `permission_suggestions` was absent for `AskUserQuestion` and `ExitPlanMode`.

## L3. Several hooks on one event

- **Documented [H].**
  - "When an event fires, its matching hooks run in parallel"; each hook's timeout starts when it starts.
  - "If you define the same handler in more than one settings file, it runs once. A plugin's or skill's copy of the same handler stays separate."
  - PreToolUse has a precedence (deny > defer > ask > allow). **No precedence is documented for `PermissionRequest`**; for an SDK host, "whichever decides first applies".
- **Verified.**
  - The probe, A and B started within ~30 ms of each other.
  - A silent hook that exits at once (tmag's own) does not delay or block a deciding hook.
  - A decision applies immediately, even while another handler still runs (3 s sleeper).
  - T4: A printed `allow`, then B printed `deny` 1.4 s later. Allow applied, and B's deny was ignored.
- **Not tested.** Conflicting decisions printed within the same instant.

## L4. Interrupt

- **Keys.** Esc ("Stop the current response or tool call mid-turn") and Ctrl+C ("Interrupts a running operation") [IM]. Both verified on a running Bash call: `Interrupted · What should Claude do instead?`.
- **Hooks on interrupt: none** (verified for Esc and for Ctrl+C mid-tool, and for Esc on a permission dialog).
  - No `Stop`, `StopFailure`, `PostToolUseFailure` or `PostToolBatch`.
  - [H] documents that `Stop` does not fire on user interrupt.
  - The next hook was `Notification` `idle_prompt`, 38.6 s after the Esc (documented as ~60 s). In a background session, `idle_prompt` came 1 s after `Stop`.
- **Esc side effects.**

  | Situation | Effect | Source |
  | --- | --- | --- |
  | Empty, idle composer, one press | no visible effect | verified |
  | Draft present, one press | does not clear; arms double-Esc | verified |
  | Draft present, two presses | clears the draft (saved to history) | verified |
  | Empty composer, two presses | opens the rewind menu (one more Esc closes it) | verified |
  | Permission or question dialog open | declines ("No" without comment) and ends the turn | [IM]; verified |
  | Footer item selected | deselects instead of interrupting | [IM] |
  | Messages queued | interrupts, then sends the queue | [IM] |
  | While `UserPromptSubmit` hooks run | cancels the prompt; text returns to the composer | [H] |

- **Ctrl+C.** At idle with a draft it clears the draft and shows `Press Ctrl-C again to exit`; a second press exits [IM]. In one test, a second press 1.5 s later on the now-empty composer did not exit, so the window's length is unknown.

## L5. Transcript

- **`transcript_path`.**
  - Common input field [H].
  - Observed on `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PreToolUse`, `PermissionRequest` (38/38), `Notification`, `PostToolUse`, `PostToolBatch` and `Stop`.
  - `SubagentStop` adds `agent_transcript_path`; `SubagentStart` has none.
  - **At a new session's first `UserPromptSubmit`, the file did not exist yet.**
- **Incremental.** Entries are appended one by one during the turn, asynchronously. Measured lags:
  - user prompt entry: ~0.5 s after `UserPromptSubmit`;
  - `assistant` `tool_use` entry: ~1.5 s after `PreToolUse`;
  - `tool_result`: ~0.4 s after `PostToolUse`;
  - final assistant text: ~0.3 s *after* `Stop` (at `Stop` the file lacked it).

  [H] warns of the lag and points to `last_assistant_message` on `Stop`.
- **Turn id.** `promptId` on every `type:"user"` entry, both the human prompt and the `tool_result` entries, equals the hooks' `prompt_id` (verified).
  - `assistant` entries carry no `promptId`; reach them through `parentUuid` → `uuid`.
  - Human prompts also carry:
    - `promptSource` (`"typed"`, also for a tmux bracketed paste);
    - `origin.kind` (`human`, or `task-notification` for a background subagent's report-back turn);
    - `turnOrigin`;
    - `turnPosition {promptIndex, turnIndex}`;
    - `permissionMode`.
  - Tool results carry `sourceToolAssistantUUID`, `toolUseResult`, `permissionDecision` (see L1) and `toolDenialKind` (`user-rejected`, `permission-rule`).
- **Other entry types.** `attachment`, `system` (`turn_duration`, `stop_hook_summary`), `api-request`, `api-request-blob`, `file-history-snapshot`, `permission-mode`, `mode`, `last-prompt`, `session-owner`, `tools-used`, `atis-latch`.
  - Per [CD], the transcript also holds the system prompt, tool definitions and file contents.
  - **Point to it; never copy from it** (tmag's "no content is stored").
- **Subagents.** `~/.claude/projects/<project>/<session_id>/subagents/agent-<agent_id>.jsonl` ([CD], [SA]); verified that `<agent_id>` equals the hook's `agent_id`.
  - Next to it, `agent-<agent_id>.meta.json` has keys `agentType`, `description`, `finished`, `isBuiltIn`, `launchedWithDefaults`, `pickedEffortModel`, `requestNonInteractive`, `requestShape`, `spawnDepth` and `toolUseId`. `toolUseId` is the parent's `Agent` tool_use id (verified).
  - Subagent entries carry `agentId` and `isSidechain: true`, and their user entries carry the **spawning turn's** `promptId`.

## L6. Background sessions

Verified with `claude --bg --plugin-dir … --setting-sources local … '<prompt>'` (the flags carry over [AV]):

- **Process tree.** The hook ran under the worker: `versions/2.1.317 --session-id …` ← `claude bg-pty-host` ← `claude daemon run`.
- **The blocking `PermissionRequest` hook approved the call with no terminal attached.** The file was created, and `PostToolUse` and `Stop` followed.
- **While the hook was pending:**
  - `claude agents --json` showed `status:"waiting"`, `waitingFor:"permission prompt"`, `state:"blocked"`;
  - `Notification` `permission_prompt` fired at +6 s;
  - `claude logs <id>` printed a raw screen dump of the session's pty (ANSI); rendered, it was the ordinary Bash dialog.
- **With `claude attach <id>` showing the dialog,** the hook's `allow` dismissed it, and the attached view showed `Allowed by PermissionRequest hook`.
- **Lifetime.** A session "paused on a permission prompt" keeps its process running; idle, unattached sessions are stopped after about an hour [AV].
- **Side note.** The supervisor exited by itself once its last session was removed.

## L7. Composer collision

- **Verified collision.**
  - Typed draft `DRAFTXYZ half typed`, then `tmux set-buffer` + `paste-buffer -p` + `send-keys Enter`.
  - Claude received **one** prompt, `DRAFTXYZ half typedReply with only the word OK.`, recorded with `promptSource:"typed"`.
- **Clearing keys** (all verified unless marked):

  | Keys | Draft present | Empty composer |
  | --- | --- | --- |
  | `C-u` | deletes cursor → line start only; text after the cursor stays | no-op |
  | `C-e C-u` | clears a single-line draft | no-op |
  | Multi-line `C-u` from the end | ~2 presses per line (text, then newline); 3 lines needed ≤ 6; lines below the cursor untouched | no-op |
  | `End`, `C-e` | move to end of the current logical line | — |
  | `C-End`, `M->` | do nothing | — |
  | `Down` | moves to the last row; on the last row it navigates history [IM] | — |
  | `C-c` | clears the whole draft (recallable with Up [IM]); shows `Press Ctrl-C again to exit` | arms exit |
  | `Esc Esc` | clears the whole draft | **opens rewind menu** |
  | `C-s` (stash) | stashes and clears | **restores an older stash** ([KB], [IM]) |
  | In `!` shell mode | — | `C-u` exits shell mode [IM] |

- **Ctrl+C** also interrupts a running turn.
- **Conclusion: no single key clears a draft both completely and harmlessly.**
  - The harmless family is `Down`×k, `C-e`, `C-u`×(2·lines+1). It needs a bound on the draft's line count. Not verified: large pastes collapsing to a `[Pasted text]` chip, and vim mode (`editorMode`), where Esc enters NORMAL mode.
  - `chat:*` keys can be rebound in `~/.claude/keybindings.json` [KB]; Ctrl+U is not a listed action.
- **Documented alternative [CSM].** Each session has an inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`, token `CLAUDE_CODE_MESSAGING_TOKEN`).
  - A message starts a turn when the session is idle, fires `UserPromptSubmit`, and never touches the composer.
  - But it arrives labeled as from another session. It "can't approve anything", commands in it don't run, and it may be held for approval in bypass-mode sessions.
  - Its wire format beyond the auth line is undocumented. Not tested.

## What this means for the dedicated blocking hook

These are facts that constrain the design, not a design:

1. **Answering races the person.** The dialog is already on screen, so printing nothing on failure loses nothing and delays nothing.
2. **The hook must learn that the person answered first.**
   - After a TUI "Yes", Claude leaves the hook running and ignores its later output, so a naive `tmag answer` would report success for nothing.
   - Esc or "No" ends the turn and SIGTERMs the hook.
   - Ways to close the request: tmag's own `PostToolUse` / `PostToolBatch` / `Stop` for the same session and agent, or SIGTERM.
   - `PermissionRequest` has no `tool_use_id`. Correlate via the preceding `PreToolUse`, or after the fact via the transcript's `hook_permission_decision.toolUseID`.
3. **Hook-denied and interrupted prompts fire no failure hook.** A deny closes on `PostToolBatch` or `Stop`; an Esc-declined prompt closes on nothing until `idle_prompt`.
4. **Defaults.**
   - Exit 0 always, and leave `onFailure` unset (`block` = deny).
   - Set an explicit `timeout`. It costs the person nothing, because the dialog is up regardless.
5. **Tools that need `updatedInput`.** `AskUserQuestion` (answers keyed by question text) and, on 2.1.317, `ExitPlanMode` need the original `tool_input` echoed back.
   - The hook process holds it in memory, so nothing needs to be stored.
   - But the person answering a question remotely needs to see its text: a tension with "no content is stored".
   - A rejected `updatedInput` denies the call.
6. **Background sessions work end to end.** This is the one channel that can answer a session no pane shows.

## Not verified

- Conflicting decisions printed at the same instant.
- Edit, WebFetch and MCP tools live.
- The TUI "No" option, as distinct from Esc.
- `interrupt:true` on deny.
- Sandbox network prompts.
- Teammates.
- Vim mode, large-paste chips, and user keybinding overrides for the composer.
- The Ctrl+C exit window.
- The inbox socket protocol.

## Cleanup done

- Stopped and removed the test background session (`claude stop` / `claude rm 61478aa5`). The supervisor it started exited by itself. The user's pre-existing sessions were not touched.
- Exited the interactive test session and killed the private tmux server (`-L research`).
- Deleted the test transcripts and subagent files: `~/.claude/projects/-tmp-tmag-claude-research-proj/`.
- Deleted the test sessions' files:
  - `~/.claude/file-history/42c1de72-…`;
  - `~/.claude/session-env/{42c1de72-…,6262acdb-…,61478aa5-…}`;
  - the plan file Claude wrote, `~/.claude/plans/the-plan-create-plan-txt-pure-anchor.md`;
  - scratchpads `/tmp/claude-1000/-tmp-tmag-claude-research-proj/`;
  - the empty `/tmp/cc-daemon-1000/`.
- User-level entries Claude wrote, removed and nothing else:
  - the workspace-trust entry `projects["/tmp/tmag-claude-research/proj"]` in `~/.claude.json`;
  - the 23 prompt-history lines with `project == "/tmp/tmag-claude-research/proj"` in `~/.claude/history.jsonl`.
- No `~/.claude/sessions/*.json` record referencing the test sessions remained.
- Deleted `/tmp/tmag-claude-research` (scratch project, plugin, logs, downloaded docs).
