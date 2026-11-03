# webui

A minimal web page over `tmag watch`, to look at agent sessions in a browser
and to check that the watch stream is all a client needs.

```sh
go run ./examples/webui                      # tmag on PATH
go run ./examples/webui -- ~/.tmux/plugins/tmux-agent-experience/bin/tmag
go run ./examples/webui -- ssh host tmag     # sessions on another machine
```

Then open <http://127.0.0.1:7777> (`-addr` to change it).

The server only relays: every page connection runs its own `tmag watch` and
receives its lines unchanged as server-sent events. Everything else happens in
the page, the way any client of the `tmag/v1` contract would do it:

- sessions come from the snapshot and every event after it, panes from the
  latest `panes` document;
- after a disconnect it reconnects with `tmag watch --since <last seq>`;
- **verify against list** compares the state it rebuilt with a fresh
  `tmag list --json`, field by field, and lists every difference.

It is an example, not part of tmag: it runs only while you run it, listens on
loopback and refuses other host names, and never sends anything to an agent.
