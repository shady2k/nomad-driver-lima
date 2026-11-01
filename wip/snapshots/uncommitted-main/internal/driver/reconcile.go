package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"time"
)

// Observed telemetry is not authenticated liveness. No timer renews the owner.
type ActivityObservation struct {
	WorkState       string    `json:"work_state"`
	LastActivityAt  time.Time `json:"last_activity_at"`
	OwnerObservedAt time.Time `json:"owner_observed_at"`
	LastOwnerEvent  string    `json:"last_owner_event"`
	Report          string    `json:"report"`
}
type LifecycleReport struct {
	Mode               string `json:"mode"`
	OwnerState         string `json:"owner_state"`
	WorkState          string `json:"work_state"`
	LeaseExpired       bool   `json:"lease_expired"`
	OrphanSuspect      bool   `json:"orphan_suspect"`
	OrphanGraceElapsed bool   `json:"orphan_grace_elapsed"`
	IdleState          string `json:"idle_state"`
	AutoClose          bool   `json:"auto_close_enabled"`
	AutoDelete         bool   `json:"auto_delete_enabled"`
}

func classifyActivity(s *State, now time.Time, unknown bool) LifecycleReport {
	a := s.Lifecycle
	r := LifecycleReport{Mode: "report-only", OwnerState: "owner_unconfirmed", WorkState: a.WorkState, IdleState: "unknown"}
	observed := a.OwnerObservedAt
	if observed.IsZero() {
		observed = s.StartedAt
	}
	// Unknown/missing or backwards host time never proves expiry or idleness.
	if !observed.IsZero() && !now.Before(observed) {
		r.LeaseExpired = now.Sub(observed) > 5*time.Minute
		r.OrphanSuspect = r.LeaseExpired
		r.OrphanGraceElapsed = now.Sub(observed) > 35*time.Minute
	}
	if s.Exit != nil {
		r.WorkState = "terminal"
		r.IdleState = "terminal"
		return r
	}
	if unknown || a.WorkState == "" || a.WorkState == "unknown" {
		r.WorkState = "unknown"
		return r
	}
	if a.WorkState == "active" || a.WorkState == "uploading" || a.WorkState == "finishing" {
		r.IdleState = "active"
		return r
	}
	if a.WorkState == "idle" && !a.LastActivityAt.IsZero() && !now.Before(a.LastActivityAt) {
		elapsed := now.Sub(a.LastActivityAt)
		r.IdleState = "recent"
		if elapsed >= 30*time.Minute {
			r.IdleState = "idle_warning"
		}
		if elapsed >= 40*time.Minute {
			r.IdleState = "idle_grace_elapsed"
		}
	}
	return r
}
func (d *Driver) observeActivity(t *task, raw []byte, now time.Time) {
	t.lifecycleBuffer = append(t.lifecycleBuffer, raw...)
	for {
		n := bytes.IndexByte(t.lifecycleBuffer, '\n')
		if n < 0 {
			break
		}
		line := t.lifecycleBuffer[:n]
		t.lifecycleBuffer = t.lifecycleBuffer[n+1:]
		var e struct {
			Component string `json:"component"`
			Stage     string `json:"stage"`
			Session   string `json:"session_id"`
		}
		if json.Unmarshal(line, &e) != nil || (e.Component != "agent-plugin" && e.Component != "agent-task") || e.Session == "" || e.Session != t.state.WorkerSessionID {
			continue
		}
		a := &t.state.Lifecycle
		switch e.Stage {
		case "guest_runtime_ready", "acp_ready", "request_terminal":
			a.WorkState = "idle"
			a.LastActivityAt = now
		case "request_running":
			a.WorkState = "active"
			a.LastActivityAt = now
			if key := lima.Hash(line); a.LastOwnerEvent != key {
				a.OwnerObservedAt = now
				a.LastOwnerEvent = key
			}
		case "result_collection", "result_upload":
			a.WorkState = "uploading"
			a.LastActivityAt = now
		case "finish_prepared", "receipt_verified":
			a.WorkState = "finishing"
		case "finish_acknowledged":
			a.WorkState = "terminal"
		case "result_upload_failed", "lifecycle_record_invalid", "runtime_check_failed":
			a.WorkState = "unknown"
		}
	}
	if len(t.lifecycleBuffer) > 1048576 {
		t.lifecycleBuffer = nil
		t.state.Lifecycle.WorkState = "unknown"
	}
}

// Called under task.mu; only change notifications, no stop/destroy or lease renewal.
func (d *Driver) reportActivity(t *task, now time.Time, unknown bool) {
	if t.state.CreatorSessionID == "" {
		return
	}
	r := classifyActivity(t.state, now, unknown)
	raw, _ := json.Marshal(r)
	signature := string(raw)
	t.state.Lifecycle.Report = signature
	if signature != t.reportSignature {
		t.reportSignature = signature
		d.emit(t.state, "lifecycle_report_only")
	}
}

// A missing recovered handle is a report, not permission to destroy a VM.
func (d *Driver) reconcileUnattached() {
	defer d.wg.Done()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	last := map[string]string{}
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
		}
		d.reconcileSnapshot(last)
	}
}

func (d *Driver) reconcileSnapshot(last map[string]string) {
	d.configMu.RLock()
	store, client := d.store, d.client
	d.configMu.RUnlock()
	if store == nil || client == nil {
		return
	}
	d.mu.Lock()
	attached := map[string]bool{}
	for _, t := range d.tasks {
		attached[t.state.Key] = true
	}
	d.mu.Unlock()
	rows, e := d.ledger()
	if e != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	infos, e := client.List(ctx)
	cancel()
	if e != nil {
		return
	}
	for _, s := range rows {
		if attached[s.Key] || s.Destroyed || s.Phase == "no_vm_failed" {
			continue
		}
		found := false
		owned := false
		for _, v := range infos {
			if v.Name == s.Instance {
				found = true
				owned = v.Param["nomad_driver_id"] == s.Owner && v.Param["nomad_task_key"] == s.Key
			}
		}
		status := "ledger_without_vm_unconfirmed"
		if found && owned {
			status = "unattached_owned_vm_owner_unconfirmed"
		} else if found {
			status = "identity_mismatch_vm_retained"
		}
		report := map[string]any{"version": 1, "mode": "report-only", "allocation": s.AllocID, "task": s.TaskID, "instance": s.Instance, "creator_agent_id": s.CreatorAgentID, "creator_session_id": s.CreatorSessionID, "state": status, "auto_close_enabled": false, "auto_delete_enabled": false}
		raw, _ := json.Marshal(report)
		if last[s.Key] == string(raw) {
			continue
		}
		last[s.Key] = string(raw)
		dir := filepath.Join(store.Root, "reports")
		if os.MkdirAll(dir, 0700) != nil {
			continue
		}
		tmp, e := os.CreateTemp(dir, ".report-")
		if e != nil {
			continue
		}
		name := tmp.Name()
		_, e = tmp.Write(raw)
		if e == nil {
			e = tmp.Sync()
		}
		tmp.Close()
		if e == nil {
			e = os.Rename(name, filepath.Join(dir, s.Key+".json"))
		}
		os.Remove(name)
		if e == nil {
			d.emit(s, "reconciliation_report_only")
		}
	}
}
