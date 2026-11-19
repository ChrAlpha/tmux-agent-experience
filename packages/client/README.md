# tmag-client

A page for your phone or tablet that shows the coding agents
[tmag](https://github.com/ChrAlpha/tmux-agent-experience) knows on a computer
you left at home, and lets you work with them: read what they were asked and
what they answered, prompt them, stop them, and answer what they ask.

A release is a directory of static files, built from this repository, that you
serve next to `tmag serve`. There is no server code. The page speaks only the
[`tmag/http/v2`](https://github.com/ChrAlpha/tmux-agent-experience/blob/main/docs/http-api.md)
contract, on its own origin under `/v2/`, and loads nothing from anywhere else.

## What it shows

- **Inbox**: every session tmag knows on each computer the page works with,
  kept current by each computer's `/v2/watch`: first those that need you
  (that ask something, or ended a turn since you last looked at them), then
  those that work, then the rest. Each says where it runs, what it does, and
  what it was last asked or last answered. A permission to run a short
  command, given as one string and nothing else, shows it whole, with Allow
  and Deny right there; anything else it asks opens where you answer it. The title counts
  what needs you. A session that works for another agent, or waits
  on one, says so.
- **Projects**: the same sessions by the project they run in, the main
  worktree of their repository (every worktree and directory of a
  repository is one project), each with a way to start another agent
  there; then the places agents ran in lately that none runs in now.
- **A session**: its last 20 turns, each the prompts it was given (including
  those from other agents and background tasks) and its reply, rendered as
  Markdown, with a way to copy it. Its header goes on to the next session
  that needs you, and starts another agent where it runs. Below them:
  - a box to prompt it, and Stop while a turn runs;
  - a card for each request it waits on: a permission (Allow, or Deny with a
    reason), a question (pick an option or write your own answer), a plan
    (Approve, or Keep planning);
  - Screen, which shows what the pane that shows the session shows, in its
    terminal's colours, with keys to answer what it asks there (a trust
    dialog, a login, a menu): tmag types them only while the pane still
    shows the screen you read;
  - Changes, what its directory changed against HEAD, file by file, each
    file's patch with its lines numbered; a comment on a line stays with
    that line as the agent changes the file, and goes into the message to
    the agent as `path:LINE: comment` with the line quoted;
  - links to the sessions it works for, and to those it waits on, as tmag
    records an agent's asks.
- **New agent**: an agent (Claude Code, Codex, Pi or oh-my-pi, or a
  template you set as `@tmag-start-NAME` in tmux) in a directory you choose
  from where agents ran lately, or type with the directories it completes
  in, optionally in a worktree of its own, with a first prompt. It opens in
  a window beside `tmag serve`'s. One that shows no session in time, at a
  trust dialog say, opens on its pane's screen, with its keys.
- **Computers**: those the page works with, how each answers, and the
  credential this device holds on each, to forget; and the way to add one.

Start, keys, changes, and an agent's asks need a tmag newer than 0.7.0, and
the directories, repositories and templates the page offers one newer than
0.8.0; on an older one the page leaves them out, and takes a directory as a
path, among those its sessions run in.

## Set up with Tailscale

You need tmag 0.7.0 or later on the computer, and [Tailscale](https://tailscale.com)
with [HTTPS certificates](https://tailscale.com/kb/1153/enabling-https) on the
computer and the phone. In the commands below, replace `box.tail1234.ts.net`
with the computer's name in your tailnet: `Self.DNSName` in
`tailscale status --json`, without its final dot.

1. Let `tmag serve` accept the page's address, in `~/.tmux.conf`:

   ```tmux
   set -g @tmag-serve '--host box.tail1234.ts.net --origin https://box.tail1234.ts.net'
   ```

   Reload the configuration and press `prefix + M-r`: tmag serve runs in a
   window of its own, on `127.0.0.1:7777`, until you close that window.

2. Get the latest release, check it, and unpack it in a directory of its own,
   here `~/tmag-client/web`:

   ```sh
   mkdir -p ~/tmag-client && cd ~/tmag-client
   curl -fsSLO https://github.com/ChrAlpha/tmag-client/releases/latest/download/tmag-client.tar.gz
   curl -fsSLO https://github.com/ChrAlpha/tmag-client/releases/latest/download/checksums.txt
   sha256sum -c checksums.txt
   rm -rf web && mkdir web && tar -xzf tmag-client.tar.gz -C web
   ```

   On macOS, check it with `shasum -a 256 -c checksums.txt`. Each archive is
   also attested to the workflow that built it: `gh attestation verify
   tmag-client.tar.gz --repo ChrAlpha/tmag-client` shows that.

3. Serve the page and the API on the tailnet:

   ```sh
   sudo tailscale serve --bg --set-path / "$HOME/tmag-client/web"
   sudo tailscale serve --bg --set-path /v2 http://127.0.0.1:7777/v2
   ```

   Tailscale keeps this configuration across restarts; `tailscale serve status`
   shows it. Without `sudo` it needs `sudo tailscale set --operator=$USER` once.
   Tailscale's macOS app cannot serve a directory: there, serve `web/` on
   loopback, for instance with `python3 -m http.server --bind 127.0.0.1 --directory ~/tmag-client/web 7778`
   in a tmux window, and give `tailscale serve --bg --set-path / http://127.0.0.1:7778`
   instead of the directory.

4. Pair the phone. On the computer run

   ```sh
   tmag auth pair phone
   ```

   then open `https://box.tail1234.ts.net` on the phone and enter the code it
   printed. A code works once, for ten minutes, and gives the phone a credential
   of its own. The page names it under Computers; `tmag auth revoke ID` on the
   computer takes it back.

   The page keeps the credential in the browser's storage. On iOS, Safari and a
   Home Screen web app each have their own, and links from notifications open in
   Safari: pair in Safari. Safari may clear the storage of a site you have not
   opened for a week; pair again then.

## Several computers

The page works with the computer that serves it and with others, each paired on
its own. The computer that serves the page reaches another's `tmag serve`
through an SSH tunnel and serves it at `/NAME/v2/`; the page stays on one
origin. For a computer `mini` that `box` reaches over SSH:

1. On `mini`, let `tmag serve` accept the page's address, as in step 1 above
   but with `box`'s name, and start it with `prefix + M-r`:

   ```tmux
   set -g @tmag-serve '--host box.tail1234.ts.net --origin https://box.tail1234.ts.net'
   ```

2. On `box`, keep a tunnel to it in a tmux window, and serve it at `/mini/v2`:

   ```sh
   tmux new-window -d -n mini-tunnel 'ssh -N -o ExitOnForwardFailure=yes -L 127.0.0.1:7779:127.0.0.1:7777 mini'
   sudo tailscale serve --bg --set-path /mini/v2 http://127.0.0.1:7779/v2
   ```

   Each other computer takes a port of its own on `box`: 7780 for the next.

3. On the page, open **Computers**, choose **Add a computer**, name it
   `mini`, and enter the code `tmag auth pair phone` prints on `mini`.

A name is lowercase letters, digits and `-`. The page will not add a computer
it already works with at another name; it tells you which credential to revoke.

## Notifications with ntfy

tmag runs `@tmag-notify` when an agent waits for you or finishes and you are not
at the terminal. To get those alerts from [ntfy](https://ntfy.sh), with a tap
that opens the session on the page, add to `~/.tmux.conf`:

```tmux
set -g @tmag-notify 'curl -fsS -m 10 -H "Title: $TMAG_TITLE" -H "Click: https://box.tail1234.ts.net/#/s/$TMAG_SESSION" -d "$TMAG_MESSAGE" https://ntfy.sh/your-topic'
```

and subscribe to the topic in the ntfy app. On another computer, put its name
in the link: `https://box.tail1234.ts.net/#/mini/s/$TMAG_SESSION`. A link
that ends in `${TMAG_REQUEST:+/r/$TMAG_REQUEST}` opens the request the agent
waits on, when it waits on one. Anyone who knows a topic's name on
ntfy.sh can read it, so choose a long random one, or run your own ntfy server. An
alert holds only what tmag knows of the session: the agent, the session's id in
the link, the tool it asks for, the directory's name, the pane and the
computer's name. `tmag notify` sends a test alert.

## Updating

Run step 2 again, then reload the page. `tailscale serve` keeps serving the
same directory.

## Developing

With Node.js 22 or later and pnpm:

```sh
pnpm install
pnpm check    # Biome, TypeScript and the unit tests
pnpm dev      # the page on http://localhost:5173, /v2/ proxied to tmag serve
```

`pnpm dev` and `pnpm preview` proxy `/v2/` to `TMAG_SERVE`
(default `http://127.0.0.1:7777`) as `tailscale serve` does, and `/NAME/v2/`
to each `NAME=URL` in `TMAG_COMPUTERS`, separated by commas; a tmag serve they
reach needs `--host localhost:5173 --origin http://localhost:5173` (4173 for
`preview`).

`TMAG_SRC=/path/to/tmux-agent-experience pnpm e2e` builds the page and runs
the end-to-end tests in WebKit and Chromium, at a phone's size, against tmag
and the fake agents built from that checkout, in private tmux servers, one for
each of two computers: nothing touches your own sessions. They need Go, tmux and
`pnpm exec playwright install chromium webkit`.

## History

This repository started on 2026-11-07 as a Go gateway extracted from tmag's
`feat/remote` branch, with its own browser admission, tunnel adapters and
multi-machine inventory. Once tmag's HTTP API carried owner credentials, pairing
and every control gate itself, the page replaced the gateway. `docs/research/herdr.md`
is the research that started the exploration.
