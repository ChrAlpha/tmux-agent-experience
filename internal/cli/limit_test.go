package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChrAlpha/tmux-agent-experience/internal/attach"
	"github.com/ChrAlpha/tmux-agent-experience/internal/eventlog"
	"github.com/ChrAlpha/tmux-agent-experience/internal/session"
)

func TestRefusalsHaveTheirOwnCodesAndHookCategory(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("refused: %w", &session.LimitError{Agent: "claude", ID: "s1", Part: "pending record", Size: 5 << 20, Limit: session.MaxRecordBytes}): "limit_exceeded",
		&eventlog.LimitError{Size: 65 << 20, Limit: eventlog.MaxRecordBytes}:                                                                            "limit_exceeded",
		&attach.MarkerError{Server: "/tmp/tmux-1000/default", Pane: "%1"}:                                                                               "attachment_unavailable",
	} {
		var stdout, stderr bytes.Buffer
		a := app{json: true, stdout: &stdout, stderr: &stderr}
		if code := a.fail("status", err); code != ExitError || !strings.Contains(stdout.String(), `"code":"`+want+`"`) {
			t.Fatalf("%v: exit %d, %s", err, code, stdout.String())
		}
	}

	root := t.TempDir()
	a := app{log: eventlog.Open(root)}
	line := time.Now().UTC().Format(time.RFC3339) + " claude limit_exceeded\n"
	if err := os.WriteFile(filepath.Join(root, "hook.log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.checkHookErrors(context.Background()); !strings.Contains(got.Detail, "latest: claude limit_exceeded") {
		t.Fatalf("hook check = %+v", got)
	}
}
