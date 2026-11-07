package driver

// The reap task: one pass over the cluster, on a node, as a periodic Nomad job.
//
// `box reap` on the coordinator runs the same pass (internal/cli). The job is a
// task of this driver for the reasons a bake and a publish are: the coordinator
// host may be a laptop, this project has no daemon, and a Nomad client already
// runs this plugin. The task runs no VM.
//
// It talks to the Nomad API through the task API: the client serves its own API
// on a unix socket at NOMAD_UNIX_ADDR, and the task authenticates with its own
// workload identity token (NOMAD_TOKEN). Both come from the client in the
// environment it builds for the task (TaskConfig.Env), not in the plugin's own
// environment, and neither is a secret of the cluster or an address in the job:
// the job sets
//
//	identity {
//	  env = true
//	}
//
// and the ACL policy of the pass is attached to the job
// (deploy/acl/box-reap.policy.hcl).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/cli"
)

// taskAPIVars are the two variables the client sets for a task that has a
// workload identity (client/taskenv: WorkloadToken, UnixAddr).
const (
	envTaskAPIAddr = "NOMAD_UNIX_ADDR"
	envTaskAPIToken = "NOMAD_TOKEN"
)

// reapOnce runs one pass and writes its lines to the task log, so the job's log
// is the record of what was deleted.
func (d *Driver) reapOnce(ctx context.Context, h *taskHandle) *drivers.ExitResult {
	log := h.workLog()
	defer log.close()
	nomad, err := nomadClientFromTask(taskEnvLookup(h.tc))
	if err != nil {
		return imageWorkFailure(h, d, err.Error())
	}
	app := cli.New(nomad, nil, nil)
	app.ErrOut = log
	if err := app.RunReap(ctx, false); err != nil {
		if ctx.Err() != nil {
			return imageWorkFailure(h, d, "the reap pass was stopped")
		}
		return imageWorkFailure(h, d, "the reap pass ran, and "+err.Error())
	}
	return &drivers.ExitResult{}
}

// taskEnvLookup looks a variable up in the environment Nomad built for the task,
// which is where the task API's address and the task's own workload identity
// token are. The plugin process has neither in its own environment: it is one
// process for every task of the node, so reading os.Getenv here would either
// find nothing or, worse, find a token that belongs to whoever started the
// client.
func taskEnvLookup(tc *drivers.TaskConfig) func(string) string {
	return func(key string) string {
		if tc == nil {
			return ""
		}
		if key == envTaskAPIAddr {
			return taskAPIAddr(tc, tc.Env[key])
		}
		return tc.Env[key]
	}
}

// taskAPIAddr turns the task API address Nomad gives a task into the one this
// process can dial.
//
// The address in the task's environment is built for the filesystem isolation
// the driver declares, and this driver declares image isolation: Nomad sets
// unix:///secrets/api.sock, the path a process started inside the task's
// filesystem would use. The pass runs here, in the plugin process on the host,
// where the same socket is <alloc dir>/<task>/secrets/api.sock -- TaskConfig
// carries the alloc directory but not the task directory, and the task
// directory is <alloc dir>/<task>.
//
// A value that is already a path this process can see is used as it is.
func taskAPIAddr(tc *drivers.TaskConfig, addr string) string {
	if tc == nil || tc.AllocDir == "" || !strings.HasPrefix(addr, "unix://") {
		return addr
	}
	socket := strings.TrimPrefix(addr, "unix://")
	if _, err := os.Stat(socket); err == nil {
		return addr
	}
	host := filepath.Join(tc.AllocDir, tc.Name, "secrets", filepath.Base(socket))
	if _, err := os.Stat(host); err != nil {
		// The socket is not where either layout says: answer the address Nomad
		// gave, so the connection error names it.
		return addr
	}
	return "unix://" + host
}

// nomadClientFromTask builds the Nomad API client of a task from the task API.
// getenv is the task's environment; a test may pass its own.
func nomadClientFromTask(getenv func(string) string) (cli.Nomad, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	addr := strings.TrimSpace(getenv(envTaskAPIAddr))
	token := strings.TrimSpace(getenv(envTaskAPIToken))
	if addr == "" || token == "" {
		return nil, fmt.Errorf("this task cannot reach the Nomad API: the job must give it a workload identity "+
			"(`identity { env = true }`), and the client then sets %s and %s for it (Nomad 2.0's task API)",
			envTaskAPIAddr, envTaskAPIToken)
	}
	cfg := api.DefaultConfig()
	cfg.Address = addr
	cfg.SecretID = token
	nomad, err := cli.NewNomadClientWithConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Nomad API of this task at %s: %w", addr, err)
	}
	return nomad, nil
}
