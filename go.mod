module github.com/ChrAlpha/tmux-agent-experience

go 1.25

// The packages are pnpm projects, whose node_modules no Go tool should walk.
ignore ./packages
