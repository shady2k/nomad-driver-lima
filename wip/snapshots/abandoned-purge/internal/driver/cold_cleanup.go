package driver

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

// cleanupTask resolves only the exact SDK task ID. It never starts a monitor,
// delivers credentials, launches main, or selects another allocation invocation.
func (d *Driver) cleanupTask(id string) (*task, func(), error) {
	noop := func() {}
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tasks[id]; t != nil {
		return t, noop, nil
	}
	key := lima.Hash([]byte(id))
	lock, err := d.store.lock(key)
	if err != nil {
		return nil, noop, err
	}
	fail := func(e error) (*task, func(), error) { unlock(lock); return nil, noop, e }
	s, err := d.store.read(key)
	if err != nil {
		return fail(fmt.Errorf("cold cleanup ledger: %w", err))
	}
	if s.TaskID != id || s.ResumeNext != nil {
		return fail(fmt.Errorf("cold cleanup task ownership mismatch"))
	}
	required := false
	if s.CleanupVaultRequired != nil {
		required = *s.CleanupVaultRequired
	} else {
		// Legacy states have no cleanup policy. Durable exposure proves a Vault
		// requirement; a worker session without it is ambiguous, never secret-free.
		exposures, e := d.store.vaultExposures(s)
		if e != nil {
			return fail(e)
		}
		required = len(exposures) > 0 || s.VaultTokenSHA256 != ""
		if !required && s.WorkerSessionID != "" {
			return fail(fmt.Errorf("cold cleanup legacy secret policy unavailable; runtime retained"))
		}
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	_, err = d.owned(ctx, s)
	cancel()
	if err != nil {
		return fail(err)
	}
	cfg := &drivers.TaskConfig{ID: id, AllocID: s.AllocID, Name: s.TaskName, Namespace: s.Namespace, NodeID: s.NodeID}
	if err = cfg.EncodeConcreteDriverConfig(TaskConfig{VaultRequired: required, WorkerSessionID: s.WorkerSessionID}); err != nil {
		return fail(err)
	}
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fail(err)
	}
	operationCtx, stop := context.WithCancel(d.ctx)
	t := &task{cfg: cfg, state: s, lock: lock, done: make(chan struct{}), operationCtx: operationCtx, cancelOperations: stop, out: out, err: out}
	d.tasks[id] = t
	return t, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.tasks[id] == t {
			delete(d.tasks, id)
		}
		stop()
		out.Close()
		unlock(lock)
	}, nil
}
