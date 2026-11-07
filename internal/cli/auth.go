package cli

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/auth"
	"github.com/ChrAlpha/tmux-agent-experience/internal/query"
)

func (a *app) cmdAuth(args []string) int {
	if len(args) == 0 {
		return a.usageError("auth", errors.New("expected issue, pair, list or revoke"))
	}
	verb := args[0]
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	fs.BoolVar(&a.json, "json", false, "")
	ttl := fs.Duration("ttl", 0, "")
	all := fs.Bool("all", false, "")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return a.usageError("auth", err)
	}
	if *ttl < 0 || (*ttl != 0 && verb != "issue" && verb != "pair") || (*all && verb != "revoke") {
		return a.usageError("auth", errors.New("--ttl belongs to issue/pair and --all to revoke"))
	}
	ctx, cancel := a.signalContext()
	defer cancel()
	// Credential mutations are a local owner's act. Proven agent tools must
	// retain their agent authority rather than minting a bearer escape hatch.
	if verb == "issue" || verb == "pair" || verb == "revoke" {
		v, w, err := query.Load(ctx, a.store())
		if err != nil {
			return a.fail("auth", err)
		}
		if v.Caller(w).By != nil {
			return a.fail("auth", &query.RefusedError{Reason: query.RefuseAgent, Detail: "manage owner credentials from the person's terminal"})
		}
	}
	store := auth.Store{Dir: auth.DefaultDir()}
	switch verb {
	case "issue", "pair":
		if len(pos) != 1 {
			return a.usageError("auth", errors.New("expected a credential label"))
		}
		if verb == "issue" {
			c, err := store.Issue(ctx, pos[0], *ttl)
			if err != nil {
				return a.fail("auth", err)
			}
			if a.json {
				a.emit(map[string]any{"credential": c})
			} else {
				fmt.Fprintf(a.stdout, "%s\n", c.Token)
				fmt.Fprintf(a.stderr, "Issued %s (%s). Save the token now; only its digest is stored.\n", c.ID, c.Label)
			}
		} else {
			p, err := store.Pair(ctx, pos[0], *ttl)
			if err != nil {
				return a.fail("auth", err)
			}
			if a.json {
				a.emit(map[string]any{"pairing": p})
			} else {
				fmt.Fprintf(a.stdout, "%s\n", p.Code)
				fmt.Fprintf(a.stderr, "Pair once before %s. A successful pairing issues its own credential.\n", p.Expires.Format(time.RFC3339))
			}
		}
	case "list":
		if len(pos) != 0 {
			return a.usageError("auth", errors.New("list takes no arguments"))
		}
		instance, list, err := store.List(ctx)
		if err != nil {
			return a.fail("auth", err)
		}
		if a.json {
			a.emit(map[string]any{"instance": instance, "credentials": list})
		} else {
			for _, c := range list {
				expiry := "until revoked"
				if c.Expires != nil {
					expiry = c.Expires.Format(time.RFC3339)
				}
				fmt.Fprintf(a.stdout, "%s\t%q\t%s\n", c.ID, c.Label, expiry)
			}
		}
	case "revoke":
		if (*all && len(pos) != 0) || (!*all && len(pos) != 1) {
			return a.usageError("auth", errors.New("revoke takes an id or --all"))
		}
		id := ""
		if len(pos) == 1 {
			id = pos[0]
		}
		if err := store.Revoke(ctx, id, *all); err != nil {
			return a.fail("auth", err)
		}
		if a.json {
			a.emit(map[string]any{"revoked": id, "all": *all})
		} else {
			fmt.Fprintln(a.stdout, "Authorization revoked.")
		}
	default:
		return a.usageError("auth", errors.New("expected issue, pair, list or revoke"))
	}
	return ExitOK
}
