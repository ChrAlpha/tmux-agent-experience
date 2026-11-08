// Command tmag is the agent-awareness companion of the tmux-agent-experience plugin.
package main

import (
	"os"

	"github.com/ChrAlpha/tmux-agent-experience/internal/cli"
	"github.com/ChrAlpha/tmux-agent-experience/internal/socketbind"
)

func main() {
	if handled, err := socketbind.RunChild(os.Args[1:]); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	os.Exit(cli.Main(os.Args[1:]))
}
