//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonGateHelpersReportSocketPathTooLongThroughShortLink(t *testing.T) {
	realRoot := filepath.Join(os.TempDir(), strings.Repeat("r", 120))
	gate := filepath.Join(realRoot, "repos", "abc.git")
	if err := os.MkdirAll(gate, 0o755); err != nil {
		t.Skipf("cannot create long root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(realRoot) })
	link := filepath.Join(t.TempDir(), "h")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", link)

	wantSocket := filepath.Join(realRoot, "socket")
	wantLength := fmt.Sprintf("is %d bytes", len(wantSocket))
	for _, args := range [][]string{
		{"daemon", "admit-push", "--gate", gate},
		{"daemon", "notify-push", "--gate", gate, "--ref", "refs/heads/x", "--old", "0", "--new", "1"},
	} {
		out, err := executeCmd(args...)
		if err == nil {
			t.Fatalf("%v: expected an error", args)
		}
		msg := out + err.Error()
		for _, want := range []string{wantSocket, wantLength, "-byte limit", "NM_HOME", "physical"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%v: output %q does not mention %q", args, msg, want)
			}
		}
	}
}

func TestDaemonStartAndStatusReportSocketPathTooLong(t *testing.T) {
	root := filepath.Join(os.TempDir(), strings.Repeat("r", 120))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Skipf("cannot create long root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("NM_HOME", root)

	originalStart, originalRunning := daemonStartFn, daemonIsRunningFn
	t.Cleanup(func() { daemonStartFn, daemonIsRunningFn = originalStart, originalRunning })
	daemonStartFn = func(*paths.Paths) error {
		t.Fatal("daemon start went past the socket path check")
		return nil
	}
	daemonIsRunningFn = func(*paths.Paths) (bool, error) {
		t.Fatal("daemon status went past the socket path check")
		return false, nil
	}

	for _, sub := range []string{"start", "status"} {
		out, err := executeCmd("daemon", sub)
		if err == nil {
			t.Fatalf("daemon %s: expected an error", sub)
		}
		msg := out + err.Error()
		for _, want := range []string{filepath.Join(root, "socket"), "NM_HOME", "limit"} {
			if !strings.Contains(msg, want) {
				t.Errorf("daemon %s: output %q does not mention %q", sub, msg, want)
			}
		}
	}
}
