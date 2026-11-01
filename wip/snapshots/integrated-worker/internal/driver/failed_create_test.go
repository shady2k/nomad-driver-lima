package driver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func failedRecoveryFixture(t *testing.T) (FailedCreateOptions, *Store, *State, failedCreateDeps) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	store, e := openStore(root)
	if e != nil {
		t.Fatal(e)
	}
	o := FailedCreateOptions{StateDir: root, LimaHome: filepath.Join(t.TempDir(), "lima"), Key: strings.Repeat("a", 64), AllocationID: "00000000-0000-4000-8000-000000000001", NodeID: "00000000-0000-4000-8000-000000000002", JobID: "worker-test", Namespace: "agent-workers"}
	s := &State{Version: 1, Key: o.Key, Owner: store.Owner, TaskID: "task-id", AllocID: o.AllocationID, TaskName: "worker", Instance: "test-aaaaaaaaaaaa", Phase: "creating", CPUs: 2, MemoryMiB: 1024, DiskGiB: 20}
	if e = store.write(s); e != nil {
		t.Fatal(e)
	}
	os.Mkdir(filepath.Join(root, "diagnostics"), 0700)
	o.FailureLog = filepath.Join(root, "diagnostics", "create-123.stderr")
	os.WriteFile(o.FailureLog, []byte("open "+filepath.Join(o.LimaHome, "_catalog-cache", strings.Repeat("b", 64)+".yaml")+": permission denied\n"), 0600)
	now := time.Now()
	deps := failedCreateDeps{now: func() time.Time { return now }, absent: func(context.Context, *State) error { return nil }, allocation: func(context.Context) (failedAllocation, error) {
		var a failedAllocation
		b, _ := json.Marshal(map[string]any{"ID": o.AllocationID, "NodeID": o.NodeID, "JobID": o.JobID, "Namespace": o.Namespace, "ClientStatus": "failed", "ModifyIndex": 123, "TaskStates": map[string]any{"worker": map[string]any{"State": "dead", "Failed": true, "Restarts": 0}}})
		json.Unmarshal(b, &a)
		return a, nil
	}}
	return o, store, s, deps
}
func assertRecoveryRetained(t *testing.T, o FailedCreateOptions, s *Store) {
	t.Helper()
	v, e := s.read(o.Key)
	if e != nil || v.Phase != "creating" || v.FailedCreateAuditSHA256 != "" {
		t.Fatal("uncertain reservation changed", v, e)
	}
}
func TestFailedCreateRecoveryAuditIdempotenceAndNoReplay(t *testing.T) {
	o, store, original, d := failedRecoveryFixture(t)
	calls := 0
	read := d.allocation
	d.allocation = func(c context.Context) (failedAllocation, error) { calls++; return read(c) }
	result, e := recoverFailedCreate(t.Context(), o, d)
	if e != nil {
		t.Fatal(e)
	}
	if calls != 2 || result.AlreadyRecovered || result.Phase != "no_vm_failed" || result.AuditSHA256 == "" {
		t.Fatal(result, calls)
	}
	budget := &Driver{store: store, config: Config{MaxVMs: 1, MaxTotalDiskGiB: 20}}
	if e := budget.checkBudget(TaskConfig{CPUs: 1, MemoryMiB: 512, DiskGiB: 20}); e != nil {
		t.Fatal("audit broke admission or reservation not released", e)
	}
	state, e := store.read(o.Key)
	if e != nil || state.CPUs != original.CPUs || state.TaskID != original.TaskID || state.Instance != original.Instance || state.Destroyed {
		t.Fatal("intent changed", state, e)
	}
	b, e := os.ReadFile(filepath.Join(store.Root, o.Key+".failed-create.audit"))
	if e != nil || lima.Hash(b) != result.AuditSHA256 {
		t.Fatal(e)
	}
	var audit failedCreateAudit
	json.Unmarshal(b, &audit)
	if audit.Original.Phase != "creating" || audit.Original.Key != o.Key || audit.AllocationModifyIndex != 123 {
		t.Fatal(audit)
	}
	again, e := recoverFailedCreate(t.Context(), o, d)
	if e != nil || !again.AlreadyRecovered || again.AuditSHA256 != result.AuditSHA256 || calls != 2 {
		t.Fatal("not idempotent", again, e)
	}
	// Simulate crash after durable audit, before ledger commit. Same original intent
	// is recovered after fresh proofs; first audit remains unchanged.
	if e = store.write(original); e != nil {
		t.Fatal(e)
	}
	again, e = recoverFailedCreate(t.Context(), o, d)
	if e != nil || again.AuditSHA256 != result.AuditSHA256 || calls != 4 {
		t.Fatal(again, e)
	}
}
func TestFailedCreateRecoveryRejectsAmbiguity(t *testing.T) {
	for _, kind := range []string{"active", "started", "restart", "wrong-node", "wrong-task", "next-allocation", "changed-index", "denied-api", "instance", "partial-disk", "process", "deadline", "phase", "machine", "diagnostic", "diagnostic-symlink", "audit-conflict", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			o, s, state, d := failedRecoveryFixture(t)
			read := d.allocation
			n := 0
			d.allocation = func(c context.Context) (failedAllocation, error) {
				a, e := read(c)
				n++
				ts := a.TaskStates["worker"]
				switch kind {
				case "active":
					a.ClientStatus = "running"
				case "started":
					ts.StartedAt = time.Now()
				case "restart":
					ts.Restarts = 1
				case "wrong-node":
					a.NodeID = "other"
				case "wrong-task":
					delete(a.TaskStates, "worker")
				case "next-allocation":
					a.NextAllocation = "next"
				case "changed-index":
					a.ModifyIndex += uint64(n)
				case "denied-api":
					return a, errors.New("403")
				}
				if kind != "wrong-task" {
					a.TaskStates["worker"] = ts
				}
				return a, e
			}
			switch kind {
			case "instance", "partial-disk", "process":
				d.absent = func(context.Context, *State) error { return errors.New("absence unproven") }
			case "deadline":
				n := 0
				now := d.now()
				d.now = func() time.Time { n++; return now.Add(time.Duration(n) * 31 * time.Second) }
			case "phase":
				state.Phase = "running"
				s.write(state)
			case "machine":
				state.MachineID = "actual"
				s.write(state)
			case "diagnostic":
				os.WriteFile(o.FailureLog, []byte("different failure"), 0600)
			case "diagnostic-symlink":
				b, _ := os.ReadFile(o.FailureLog)
				os.Remove(o.FailureLog)
				target := filepath.Join(t.TempDir(), "log")
				os.WriteFile(target, b, 0600)
				os.Symlink(target, o.FailureLog)
			case "audit-conflict":
				os.WriteFile(filepath.Join(s.Root, o.Key+".failed-create.audit"), []byte("{}"), 0600)
			}
			ctx := t.Context()
			if kind == "canceled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if _, e := recoverFailedCreate(ctx, o, d); e == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if kind != "phase" {
				assertRecoveryRetained(t, o, s)
			}
		})
	}
}
func TestFailedCreateRecoveryLiveTaskLockAndAdmissionLock(t *testing.T) {
	for _, admission := range []bool{false, true} {
		o, s, _, d := failedRecoveryFixture(t)
		key := o.Key
		if admission {
			key = lima.Hash([]byte("node-admission"))
		}
		lock, e := s.lock(key)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = recoverFailedCreate(t.Context(), o, d); e == nil {
			t.Fatal("concurrent owner lock bypassed")
		}
		unlock(lock)
		assertRecoveryRetained(t, o, s)
	}
}

func TestLinuxRecoveryActualCLIAndProcessProof(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("disposable root Linux fixture only")
	}
	for _, kind := range []string{"success", "stdio", "invalid-json-exit-zero", "partial-disk", "process"} {
		t.Run(kind, func(t *testing.T) {
			root, e := os.MkdirTemp("/tmp", "recovery-cli-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(root)
			os.Chmod(root, 0755)
			o, store, _, d := failedRecoveryFixture(t)
			// Put the fixture under an accessible parent for the dropped runtime UID.
			o.LimaHome = filepath.Join(root, "lima")
			os.Mkdir(o.LimaHome, 0700)
			os.Chown(o.LimaHome, 65534, 65534)
			os.WriteFile(o.FailureLog, []byte("open "+filepath.Join(o.LimaHome, "_catalog-cache", strings.Repeat("b", 64)+".yaml")+": permission denied\n"), 0600)
			o.RuntimeUser = "nobody"
			o.LimaBinary = filepath.Join(root, "limactl")
			os.WriteFile(o.LimaBinary, []byte("#!/bin/sh\n[ \"$1\" = list ] && [ \"$2\" = --json ] || exit 33\n"), 0755)
			a, _ := d.allocation(t.Context())
			b, _ := json.Marshal(a)
			body := "#!/bin/sh\n[ \"$1\" = operator ] && [ \"$2\" = api ] && [ \"$4\" = -X ] && [ \"$5\" = GET ] || exit 34\ncat <<'JSON'\n" + string(b) + "\nJSON\n"
			if kind == "invalid-json-exit-zero" {
				body = "#!/bin/sh\nprintf 'Invalid method'\n"
			}
			o.NomadBinary = filepath.Join(root, "nomad")
			os.WriteFile(o.NomadBinary, []byte(body), 0755)
			if kind == "partial-disk" {
				os.Mkdir(filepath.Join(o.LimaHome, "test-aaaaaaaaaaaa"), 0700)
			}
			if kind == "process" {
				processPath := filepath.Join(root, "test-aaaaaaaaaaaa")
				os.Symlink("/bin/sleep", processPath)
				cmd := exec.Command(processPath, "30")
				cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{65534}}}
				if e = cmd.Start(); e != nil {
					t.Fatal(e)
				}
				defer func() { cmd.Process.Kill(); cmd.Wait() }()
			}
			var result FailedCreateResult
			if kind == "stdio" {
				in, controller := io.Pipe()
				requests, out := io.Pipe()
				defer in.Close()
				defer controller.Close()
				defer requests.Close()
				defer out.Close()
				o.NomadBinary = ""
				controllerDone := make(chan error, 1)
				go func() {
					dec := json.NewDecoder(requests)
					for seq := 1; seq <= 2; seq++ {
						var challenge recoveryChallenge
						if e := dec.Decode(&challenge); e != nil {
							controllerDone <- e
							return
						}
						if challenge.Sequence != seq {
							controllerDone <- errors.New("wrong sequence")
							return
						}
						reply := recoveryObservation{1, "allocation_observation", seq, challenge.Nonce, challenge.Target, time.Now(), a}
						if e := json.NewEncoder(controller).Encode(reply); e != nil {
							controllerDone <- e
							return
						}
					}
					controllerDone <- nil
				}()
				result, e = RecoverFailedCreateStdio(t.Context(), o, in, out)
				if e == nil {
					if ce := <-controllerDone; ce != nil {
						t.Fatal(ce)
					}
				}
			} else {
				result, e = RecoverFailedCreate(t.Context(), o)
			}
			if kind == "success" || kind == "stdio" {
				if e != nil || result.Phase != "no_vm_failed" {
					t.Fatal(result, e)
				}
			} else {
				if e == nil {
					t.Fatal("ambiguous process/disk/API accepted")
				}
				if kind == "process" && !strings.Contains(e.Error(), "matching runtime process exists") {
					t.Fatal("process rejection not proven", e)
				}
				assertRecoveryRetained(t, o, store)
			}
		})
	}
}
