package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/catalog"
)

func exceeds(n, limit int) bool { return limit > 0 && n > limit }
func (d *Driver) catalogEntry(tc TaskConfig) (catalog.Entry, error) {
	c, e := catalog.Read(d.config.CatalogFile, d.config.CatalogSHA256)
	if e != nil {
		return catalog.Entry{}, e
	}
	vm := "qemu"
	if runtime.GOOS == "darwin" {
		vm = "vz"
	}
	v, e := c.Select(tc.TemplateID, tc.TemplateVersion, tc.Architecture, vm)
	if e != nil {
		return v, e
	}
	if tc.TemplateDiskSHA256 != v.Disk.SHA256 {
		return v, fmt.Errorf("task disk digest differs from approved template")
	}
	if tc.DiskGiB < v.Disk.MinimumDiskGiB {
		return v, fmt.Errorf("disk request below prepared image minimum")
	}
	return v, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validateCreator(tc TaskConfig) error {
	if !uuidPattern.MatchString(tc.CreatorAgentID) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`).MatchString(tc.CreatorSessionID) || !uuidPattern.MatchString(tc.WorkerSessionID) || !slugPattern.MatchString(tc.Purpose) || (tc.Project != "" && !slugPattern.MatchString(tc.Project)) {
		return fmt.Errorf("originating agent UUID, native session ID, worker UUID and purpose slug required")
	}
	if len(tc.TaskLink) > 1024 || strings.ContainsAny(tc.TaskLink, "\r\n\x00") {
		return fmt.Errorf("task link invalid")
	}
	return nil
}
func instanceName(tc TaskConfig, key string) string {
	if tc.Purpose == "" {
		return "nomad-" + key[:24]
	}
	prefix := tc.Purpose
	if tc.Project != "" {
		prefix = tc.Project + "-" + prefix
	}
	if len(prefix) > 17 {
		prefix = strings.TrimRight(prefix[:17], "-")
	}
	return prefix + "-" + key[:12]
}
func (d *Driver) ledger() ([]*State, error) {
	paths, e := filepath.Glob(filepath.Join(d.store.Root, "*.json"))
	if e != nil {
		return nil, e
	}
	out := []*State{}
	for _, p := range paths {
		k := strings.TrimSuffix(filepath.Base(p), ".json")
		s, e := d.store.read(k)
		if e != nil {
			return nil, fmt.Errorf("ledger unreadable; admission retained")
		}
		out = append(out, s)
	}
	return resumeLedger(out, d.store)
}

// Caller holds a process-wide admission mutex and a cross-process store lock.
// Retained disks consume disk budget even after DestroyTask. Unknown intents count.
func (d *Driver) checkBudget(tc TaskConfig) error {
	rows, e := d.ledger()
	if e != nil {
		return e
	}
	count, cpu, mem, disk := int64(1), int64(tc.CPUs), int64(tc.MemoryMiB+d.config.MemoryOverheadMiB), int64(tc.DiskGiB)
	for _, s := range rows {
		if purged, e := d.store.purged(s); e != nil {
			return e
		} else if purged {
			continue
		}
		if s.Phase == "no_vm_failed" {
			continue
		}
		disk += int64(s.DiskGiB)
		if !s.Destroyed && s.Phase != "stopped" {
			count++
			cpu += int64(s.CPUs)
			mem += int64(s.MemoryMiB + d.config.MemoryOverheadMiB)
		}
	}
	physical, err := hostMemoryMiB()
	if err != nil {
		return err
	}
	committed, err := d.runtimeResourceCommitment(rows)
	if err != nil {
		return err
	}
	count = committed.VMs + 1
	cpu = committed.CPUs + int64(tc.CPUs)
	mem = committed.MemoryMiB + int64(tc.MemoryMiB+d.config.MemoryOverheadMiB)
	if mem > physical {
		return fmt.Errorf("host physical RAM commitment exceeded: requested aggregate %d MiB, physical %d MiB", mem, physical)
	}
	for _, v := range [][2]int64{{count, int64(d.config.MaxVMs)}, {cpu, int64(d.config.MaxTotalCPUs)}, {mem, int64(d.config.MaxTotalMemoryMiB)}, {disk, int64(d.config.MaxTotalDiskGiB)}} {
		if v[1] > 0 && v[0] > v[1] {
			return fmt.Errorf("node operator quota exceeded (retained/uncertain resources included)")
		}
	}
	return nil
}
func (d *Driver) uniqueIdentity(s *State, machine string) error {
	if s.TemplateID == "" && s.PrivateCopy == nil {
		return nil
	}
	rows, e := d.ledger()
	if e != nil {
		return e
	}
	for _, other := range rows {
		if other.Key != s.Key && other.MachineID == machine {
			return fmt.Errorf("duplicate guest machine identity; VM retained, no workload launch")
		}
	}
	return nil
}

// Control stages are native allocation logs even before the guest main process.
// Payload is structured, bounded and restricted to non-secret progress counters.
func (d *Driver) stage(cfg *drivers.TaskConfig, s *State, phase string, counts map[string]any) error {
	now := time.Now()
	v := map[string]any{"component": "lima", "stage": phase, "time": now.UTC().Format(time.RFC3339Nano), "instance": s.Instance, "allocation": s.AllocID}
	if s.TemplateCatalogSHA256 != "" {
		v["resolved_template"] = selectionAnnotations(s, phase)
	}
	if s.StageName != phase {
		if !s.StageStartedAt.IsZero() {
			v["previous_stage"] = s.StageName
			v["previous_duration_ms"] = now.Sub(s.StageStartedAt).Milliseconds()
		}
		s.StageName = phase
		s.StageStartedAt = now
	}
	v["stage_elapsed_ms"] = now.Sub(s.StageStartedAt).Milliseconds()
	for _, k := range []string{"bytes", "total_bytes"} {
		if n, ok := counts[k]; ok {
			v[k] = n
		}
	}
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f := newTaskLog(d.ctx, cfg.StdoutPath)
	_, e = f.Write(append(raw, '\n'))
	f.Close()
	d.emit(s, phase)
	return e
}

// guardAllocationInvocation runs under node-admission. Nomad may allocate a new
// invocation ID after RecoverTask fails even though the allocation is unchanged.
// A lost handle is never permission to create another VM for the same task.
func (d *Driver) guardAllocationInvocation(cfg *drivers.TaskConfig) error {
	rows, err := d.ledger()
	if err != nil {
		return err
	}
	for _, s := range rows {
		if s.TaskID == cfg.ID || s.AllocID != cfg.AllocID || s.TaskName != cfg.Name || s.Namespace != cfg.Namespace {
			continue
		}
		if s.Phase == "no_vm_failed" {
			continue
		}
		purged, err := d.store.purged(s)
		if err != nil {
			return err
		}
		if purged {
			continue
		}
		// A phase/exit flag alone is not a physical stop receipt. In particular,
		// stale destroyed state must not authorize a duplicate of a surviving VM.
		if s.StopReceipt == nil || !s.StopReceipt.RevocationConfirmed {
			return fmt.Errorf("allocation task already has a retained or uncertain invocation; recover or confirm cleanup before starting another VM")
		}
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		v, err := d.owned(ctx, s)
		cancel()
		if err != nil || v.Status != "Stopped" || v.DriverPID != 0 || v.HostAgentPID != 0 {
			return fmt.Errorf("previous allocation task runtime is not confirmed stopped; new VM creation refused")
		}
	}
	return nil
}
