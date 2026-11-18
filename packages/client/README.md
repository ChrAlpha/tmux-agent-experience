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

- **Agents**: every session tmag knows, blocked first, then working, then
  idle, kept current by `/v2/watch`. The title counts the blocked ones.
- **A session**: its last 20 turns, each the prompts it was given (including
  those from other agents and background tasks) and its reply, rendered as
  Markdown. Below them:
  - a box to prompt it, and Stop while a turn runs;
  - a card for each request it waits on: a permission (Allow, or Deny with a
    reason), a question (pick an option or write your own answer), a plan
    (Approve, or Keep planning);
  - Screen, which shows the text of the pane that shows the session.

To start new work, prompt an agent that runs: it starts peers in tmux itself.

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
   of its own. The page names it at the foot of the agents' list; `tmag auth
   revoke ID` on the computer takes it back.

   The page keeps the credential in the browser's storage. On iOS, Safari and a
   Home Screen web app each have their own, and links from notifications open in
   Safari: pair in Safari. Safari may clear the storage of a site you have not
   opened for a week; pair again then.

## Notifications with ntfy

tmag runs `@tmag-notify` when an agent waits for you or finishes and you are not
at the terminal. To get those alerts from [ntfy](https://ntfy.sh), with a tap
that opens the session on the page, add to `~/.tmux.conf`:

```tmux
set -g @tmag-notify 'curl -fsS -m 10 -H "Title: $TMAG_TITLE" -H "Click: https://box.tail1234.ts.net/#/s/$TMAG_SESSION" -d "$TMAG_MESSAGE" https://ntfy.sh/your-topic'
```

and subscribe to the topic in the ntfy app. Anyone who knows a topic's name on
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
(default `http://127.0.0.1:7777`) as `tailscale serve` does; a tmag serve they
reach needs `--host localhost:5173 --origin http://localhost:5173` (4173 for
`preview`).

`TMAG_SRC=/path/to/tmux-agent-experience pnpm e2e` builds the page and runs
the end-to-end tests in WebKit and Chromium, at a phone's size, against tmag
and the fake agents built from that checkout, in a private tmux server: nothing
touches your own sessions. They need Go, tmux and
`pnpm exec playwright install chromium webkit`.

## History

This repository started on 2026-11-07 as a Go gateway extracted from tmag's
`feat/remote` branch, with its own browser admission, tunnel adapters and
multi-machine inventory. Once tmag's HTTP API carried owner credentials, pairing
and every control gate itself, the page replaced the gateway. `docs/research/herdr.md`
is the research that started the exploration.
