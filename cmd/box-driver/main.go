// Command box-driver is the Nomad task driver plugin for box VMs: one Lima
// instance per task, with its disk on a Nomad dynamic host volume.
package main

import (
	"context"
	"os"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/shady2k/nomad-driver-lima/internal/driver"
)

func main() {
	// A plugin process must not outlive the Nomad client that started it.
	//
	// When the client is killed, this process is orphaned, and a restarted client
	// may reattach to it before its old connection closes; the plugin then exits
	// under the new client, which leaves Nomad with a driver handle that answers
	// "plugin is shut down" and fails the task restore (measured with Nomad
	// v2.0.7). Exiting as soon as the parent is gone makes the client start a
	// fresh plugin instance instead of adopting a dying one.
	watchParent()
	plugins.ServeCtx(func(ctx context.Context, log hclog.Logger) any {
		return driver.NewDriver(ctx, log)
	})
}

// watchParent exits the process as soon as the Nomad client that started it is
// gone (the process is reparented), polling fast enough to win the race against
// the client's own restart.
func watchParent() {
	parent := os.Getppid()
	if parent <= 1 {
		// Started by an init or a shell that already exited; nothing to watch.
		return
	}
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if os.Getppid() != parent {
				os.Exit(0)
			}
		}
	}()
}
