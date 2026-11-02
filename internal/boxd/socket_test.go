package boxd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSocketIsOpenToTheBoxUser covers the permission the box CLI depends on.
//
// `nomad alloc exec` runs guest commands as the box user (the lima user), not as
// root, and every guest call of the CLI is `boxctl`. With a root-only socket the
// CLI could not talk to its own daemon, so the socket is world-accessible and
// its directory is traversable. The box is a single-tenant VM: boxd is the only
// writer.
func TestSocketIsOpenToTheBoxUser(t *testing.T) {
	cfg := testConfig(t)
	// The daemon creates the socket directory with mode 0755; t.TempDir() is
	// 0700, so use a path below it to check what the daemon sets up.
	runDir := filepath.Join(cfg.StateDir, "run")
	cfg.Socket = filepath.Join(runDir, "boxd.sock")

	daemon, err := NewDaemon(cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				t.Errorf("the daemon stopped early: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	var sockInfo, dirInfo os.FileInfo
	for time.Now().Before(deadline) {
		if sockInfo, err = os.Stat(cfg.Socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("socket never appeared: %v", err)
	}
	if got, want := sockInfo.Mode().Perm(), os.FileMode(0o666); got != want {
		t.Errorf("socket mode = %o, want %o: an unprivileged boxctl could not connect", got, want)
	}
	if dirInfo, err = os.Stat(runDir); err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if got, want := dirInfo.Mode().Perm()&0o111, os.FileMode(0o111); got != want {
		t.Errorf("socket directory mode = %o, want it traversable (0755)", dirInfo.Mode().Perm())
	}
}
