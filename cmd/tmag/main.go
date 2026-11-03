// Command tmag is the agent-awareness companion of the tmux-agent-experience plugin.
package main

import (
	"os"

	"github.com/ChrAlpha/tmux-agent-experience/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
