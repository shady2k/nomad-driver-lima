package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ns "github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
)

func changedConfig(t *testing.T, cfg *drivers.TaskConfig, cpu, memory, disk int) {
	t.Helper()
	var tc TaskConfig
	cfg.DecodeDriverConfig(&tc)
	tc.CPUs, tc.MemoryMiB, tc.DiskGiB = cpu, memory, disk
	tc.ResourceChangeReason = "bounded workload requires desired capacity"
	if e := cfg.EncodeConcreteDriverConfig(tc); e != nil {
		t.Fatal(e)
	}
	cfg.Resources.NomadResources = &ns.AllocatedTaskResources{Cpu: ns.AllocatedCpuResources{CpuShares: int64(cpu * 1000)}, Memory: ns.AllocatedMemoryResources{MemoryMB: int64(memory + 128)}}
}
func TestResourceChangeNewNomadReservationAndSameData(t *testing.T) {
	d, f, old, next, source, _ := stoppedResumeFixture(t)
	changedConfig(t, next, 2, 768, 2)
	d.config.MaxVMs, d.config.MaxTotalCPUs, d.config.MaxTotalMemoryMiB, d.config.MaxTotalDiskGiB = 1, 2, 896, 2
	disk := filepath.Join(d.config.LimaHome, source.Instance, "disk")
	before, _ := os.ReadFile(disk)
	if _, _, e := d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	s, e := d.store.read(lima.Hash([]byte(next.ID)))
	if e != nil {
		t.Fatal(e)
	}
	if s.ResourceChangePhase != "hardware_rootfs_verified" || s.ResourceChange == nil || s.CPUs != 2 || s.MemoryMiB != 768 || s.DiskGiB != 2 {
		t.Fatal("desired resources not committed", s)
	}
	after, _ := os.ReadFile(disk)
	if string(before) != string(after) {
		t.Fatal("old data was rewritten")
	}
	if e = d.store.resourceConfigProof(s); e != nil {
		t.Fatal(e)
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].DiskGiB != 2 {
		t.Fatal("resource quota not transactional", rows, e)
	}
	if e = d.StopTask(old.ID, time.Second, ""); e == nil {
		t.Fatal("old handle can stop changed VM")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.edits != 1 || f.creates != 1 || f.starts != 2 || f.launches != 2 || f.deletes != 0 {
		t.Fatal("unexpected native effects", f.edits, f.creates, f.starts, f.launches)
	}
}
func TestResourceChangeCPURAMReductionAfterStop(t *testing.T) {
	d, f, _, next, _, _ := stoppedResumeFixture(t)
	changedConfig(t, next, 3, 1024, 2)
	if _, _, e := d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	if e := d.StopTask(next.ID, time.Second, "SIGTERM"); e != nil {
		t.Fatal(e)
	}
	if e := d.DestroyTask(next.ID, false); e != nil {
		t.Fatal(e)
	}
	third, token := tokenFixture(t)
	third.ID = "reduction/work/attempt"
	third.AllocID = "00000000-0000-4000-8000-000000000008"
	third.NodeID = next.NodeID
	third.Namespace = next.Namespace
	var tc TaskConfig
	next.DecodeDriverConfig(&tc)
	tc.ResumeFromKey = lima.Hash([]byte(next.ID))
	tc.ResumeGeneration = 2
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000009"
	third.EncodeConcreteDriverConfig(tc)
	changedConfig(t, third, 1, 512, 2)
	os.WriteFile(token, []byte("synthetic-third-token"), 0600)
	f.mu.Lock()
	f.phase = "running"
	f.mu.Unlock()
	if _, _, e := d.StartTask(third); e != nil {
		t.Fatal(e)
	}
	s, e := d.store.read(lima.Hash([]byte(third.ID)))
	if e != nil || s.CPUs != 1 || s.MemoryMiB != 512 || s.DiskGiB != 2 {
		t.Fatal("reduction mismatch", s, e)
	}
	rows, e := d.ledger()
	if e != nil || len(rows) != 1 || rows[0].DiskGiB != 2 {
		t.Fatal(rows, e)
	}
}
func TestResourceChangeRejectsBeforeEffects(t *testing.T) {
	for _, fault := range []string{"quota", "guest-limit", "reservation", "physical", "missing-root-proof", "running", "no-reason", "no-op", "unsupported-vm"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, source, _ := stoppedResumeFixture(t)
			changedConfig(t, next, 2, 768, 2)
			switch fault {
			case "quota":
				d.config.MaxTotalDiskGiB = 1
			case "guest-limit":
				d.config.MaxGuestCPUs = 1
			case "reservation":
				next.Resources.NomadResources.Cpu.CpuShares = 1000
			case "physical":
				d.config.DiskFreeFloorMiB = 1048576
			case "missing-root-proof":
				source.StopReceipt.Resources = nil
				d.store.write(source)
			case "running":
				f.mu.Lock()
				v := f.infos[source.Instance]
				v.Status = "Running"
				f.infos[source.Instance] = v
				f.mu.Unlock()
			case "no-reason":
				var tc TaskConfig
				next.DecodeDriverConfig(&tc)
				tc.ResourceChangeReason = ""
				next.EncodeConcreteDriverConfig(tc)
			case "no-op":
				changedConfig(t, next, 1, 512, 1)
			case "unsupported-vm":
				source.ResolvedVMType = "external"
				d.store.write(source)
			}
			if _, _, e := d.StartTask(next); e == nil {
				t.Fatal("unsafe resource change accepted")
			}
			unchanged, e := d.store.read(source.Key)
			if e != nil || unchanged.ResumeNext != nil {
				t.Fatal("failed admission transferred ownership", e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.edits != 0 || f.starts != 1 || f.launches != 1 {
				t.Fatal("rejected change had hardware effects")
			}
		})
	}
	source, next := resumeProjectionStates(strings.Repeat("a", 32))
	source.StopReceipt = &StopReceipt{}
	source.DiskGiB = 20
	tc := TaskConfig{CPUs: 1, MemoryMiB: 512, DiskGiB: 19, ResourceChangeReason: "shrink"}
	d := &Driver{config: Config{ExpectedVersion: "2.2.1"}}
	if _, e := d.prepareResourceChange(source, tc); e == nil || !strings.Contains(e.Error(), "shrink") {
		t.Fatal("disk shrink accepted", next, e)
	}
}
func TestResourceChangeLostEditResponseReconcilesWithoutReediting(t *testing.T) {
	for _, fault := range []string{"before-edit", "after-edit", "after-audit"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, _, _ := stoppedResumeFixture(t)
			changedConfig(t, next, 2, 768, 2)
			if fault == "after-audit" {
				tc, spec, e := d.validate(next)
				if e != nil {
					t.Fatal(e)
				}
				key := lima.Hash([]byte(next.ID))
				s, e := d.prepareResume(next, tc, spec, key)
				if e != nil {
					t.Fatal(e)
				}
				old, _ := json.Marshal(s)
				if e = d.applyResourceConfig(t.Context(), s); e != nil {
					t.Fatal(e)
				}
				// Crash after audit fsync but before successor-row commit.
				if e = os.WriteFile(filepath.Join(d.store.Root, key+".json"), old, 0600); e != nil {
					t.Fatal(e)
				}
			} else {
				var once sync.Once
				runner := &resumeFaultRunner{base: f}
				if fault == "before-edit" {
					runner.before = func(_ context.Context, _ map[string]any, args []string) error {
						injected := false
						if args[0] == "edit" {
							once.Do(func() { injected = true })
						}
						if injected {
							return fmt.Errorf("injected before edit")
						}
						return nil
					}
				}
				if fault == "after-edit" {
					runner.after = func(_ map[string]any, args []string, b []byte, e error) ([]byte, error) {
						injected := false
						if args[0] == "edit" {
							once.Do(func() { injected = true })
						}
						if injected {
							return nil, fmt.Errorf("injected lost edit response")
						}
						return b, e
					}
				}
				d.client.Runner = runner
				_, _, e := d.StartTask(next)
				if fault == "before-edit" {
					if e == nil {
						t.Fatal("fault not injected")
					}
					rows, e := d.ledger()
					if e != nil || len(rows) != 1 || rows[0].DiskGiB != 2 {
						t.Fatal("pending change lost reservation", rows, e)
					}
				} else if e != nil {
					t.Fatal(e)
				}
				if fault == "after-edit" {
					return
				}
			}
			if _, _, e := d.StartTask(next); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.edits != 1 || f.starts != 2 || f.launches != 2 {
				t.Fatal("repeated edit/boot/workload", f.edits, f.starts, f.launches)
			}
		})
	}
}
func TestResourceChangeWrongHardwareOrRootFSNeverGetsToken(t *testing.T) {
	for _, fault := range []string{"cpu", "memory", "rootfs", "uuid", "unrelated-setting"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, source, _ := stoppedResumeFixture(t)
			changedConfig(t, next, 2, 768, 2)
			ctx, cancel := context.WithTimeout(d.ctx, 150*time.Millisecond)
			defer cancel()
			d.ctx = ctx
			f.mu.Lock()
			syncs := f.vaultSyncs
			f.mu.Unlock()
			d.client.Runner = &resumeFaultRunner{base: f, after: func(p map[string]any, args []string, b []byte, e error) ([]byte, error) {
				if fault == "unrelated-setting" && args[0] == "edit" {
					file := filepath.Join(d.config.LimaHome, source.Instance, "lima.yaml")
					raw, _ := os.ReadFile(file)
					os.WriteFile(file, append(raw, []byte("mounts: [changed]\n")...), 0600)
				}
				if p["action"] == "resource_status" {
					var raw map[string]any
					json.Unmarshal(b, &raw)
					var r lima.ResourceObservation
					rb, _ := json.Marshal(raw["resources"])
					json.Unmarshal(rb, &r)
					switch fault {
					case "cpu":
						r.CPUs = 1
					case "memory":
						r.MemoryBytes = 128 * 1048576
					case "rootfs":
						r.FilesystemBytes = source.StopReceipt.Resources.FilesystemBytes
					case "uuid":
						r.FSUUID = "reformatted"
					}
					raw["resources"] = r
					b, _ = json.Marshal(raw)
				}
				return b, e
			}}
			if _, _, e := d.StartTask(next); e == nil {
				t.Fatal("unverified hardware/filesystem accepted")
			}
			rows, e := d.ledger()
			if e != nil || len(rows) != 1 || rows[0].DiskGiB != 2 {
				t.Fatal("unknown outcome released quota", rows, e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.vaultSyncs != syncs || f.launches != 1 {
				t.Fatal("workload or credentials reached unverified capacity")
			}
		})
	}
}

func TestResourceChangeAuditLossNeverAdvertisesFreeQuota(t *testing.T) {
	d, _, _, next, _, _ := stoppedResumeFixture(t)
	changedConfig(t, next, 2, 513, 2)
	if _, _, e := d.StartTask(next); e != nil {
		t.Fatal(e)
	}
	s, e := d.store.read(lima.Hash([]byte(next.ID)))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(d.store.Root, s.Key+".resource-config.audit")); e != nil {
		t.Fatal(e)
	}
	if _, e = d.ledger(); e == nil {
		t.Fatal("missing resize proof accepted")
	}
	if e = d.checkBudget(TaskConfig{CPUs: 1, MemoryMiB: 512, DiskGiB: 1}); e == nil {
		t.Fatal("unknown quota admitted work")
	}
}
func TestResourceChangeIntentAndRequestedReservationCannotDrift(t *testing.T) {
	d, _, _, next, _, _ := stoppedResumeFixture(t)
	changedConfig(t, next, 2, 768, 2)
	tc, hash, e := d.validate(next)
	if e != nil {
		t.Fatal(e)
	}
	s, e := d.prepareResume(next, tc, hash, lima.Hash([]byte(next.ID)))
	if e != nil {
		t.Fatal(e)
	}
	mutated := *s
	change := *s.ResourceChange
	change.Reason = "changed"
	mutated.ResourceChange = &change
	if e = d.store.write(&mutated); e == nil {
		t.Fatal("resource intent overwritten")
	}
	// Corrupt a non-intent field directly: RPC validation must reject it before
	// any native edit even if a stored spec digest was left unchanged.
	mutated = *s
	mutated.CPUs = 4
	raw, _ := json.Marshal(mutated)
	os.WriteFile(filepath.Join(d.store.Root, s.Key+".json"), raw, 0600)
	if _, _, e = d.StartTask(next); e == nil {
		t.Fatal("hardware no longer covered by allocation accepted")
	}
}

func TestResourceChangeVerifiedPreCredentialFailureStopsNewAttempt(t *testing.T) {
	d, f, _, next, _, _ := stoppedResumeFixture(t)
	changedConfig(t, next, 2, 768, 2)
	f.mu.Lock()
	syncs := f.vaultSyncs
	f.mu.Unlock()
	var once sync.Once
	d.client.Runner = &resumeFaultRunner{base: f, after: func(p map[string]any, _ []string, b []byte, e error) ([]byte, error) {
		if p["action"] == "resource_status" {
			once.Do(func() { os.Remove(next.StdoutPath); os.Mkdir(next.StdoutPath, 0700) })
		}
		return b, e
	}}
	if _, _, e := d.StartTask(next); e == nil {
		t.Fatal("post-proof log failure not injected")
	}
	s, e := d.store.read(lima.Hash([]byte(next.ID)))
	if e != nil || s.ResourceCredentialDeliveryStarted || s.MachineID != "" || s.ResourceChangePhase != "unverified_boot_stopped" {
		t.Fatal("pre-credential cleanup not durable", e)
	}
	f.mu.Lock()
	v := f.infos[s.Instance]
	if v.Status != "Stopped" || f.vaultSyncs != syncs || f.launches != 1 {
		f.mu.Unlock()
		t.Fatal("failed verified boot leaked hardware/token/workload")
	}
	f.mu.Unlock()
	os.Remove(next.StdoutPath)
	os.WriteFile(next.StdoutPath, nil, 0600)
	if _, _, e = d.StartTask(next); e != nil {
		t.Fatal("known unlaunched retry failed", e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.edits != 1 || f.launches != 2 {
		t.Fatal("retry replayed config/workload")
	}
}
func TestResourceChangeRecoverRefusesPartialLaunchAndMissingProof(t *testing.T) {
	for _, fault := range []string{"partial-launch", "missing-audit", "missing-observation"} {
		t.Run(fault, func(t *testing.T) {
			d, f, _, next, _, _ := stoppedResumeFixture(t)
			changedConfig(t, next, 2, 768, 2)
			if fault == "partial-launch" {
				d.client.Runner = &resumeFaultRunner{base: f, after: func(p map[string]any, _ []string, b []byte, e error) ([]byte, error) {
					if p["action"] == "launch" {
						return nil, fmt.Errorf("lost launch acknowledgment")
					}
					return b, e
				}}
			}
			_, _, startErr := d.StartTask(next)
			if fault == "partial-launch" {
				if startErr == nil {
					t.Fatal("launch fault not injected")
				}
			} else if startErr != nil {
				t.Fatal(startErr)
			}
			s, e := d.store.read(lima.Hash([]byte(next.ID)))
			if e != nil {
				t.Fatal(e)
			}
			h := drivers.NewTaskHandle(1)
			h.Config = next
			h.SetDriverState(map[string]string{"key": s.Key, "owner": s.Owner, "spec": s.SpecHash})
			if e = d.Shutdown(context.Background()); e != nil {
				t.Fatal(e)
			}
			if fault == "missing-audit" {
				os.Remove(filepath.Join(d.store.Root, s.Key+".resource-config.audit"))
			}
			if fault == "missing-observation" {
				s.ResourceObservation = nil
				d.store.write(s)
			}
			recovered := setup(t, f, filepath.Dir(d.config.StateDir))
			recovered.config.VaultAddress = d.config.VaultAddress
			f.mu.Lock()
			syncs := f.vaultSyncs
			launches := f.launches
			f.mu.Unlock()
			if e = recovered.RecoverTask(h); e == nil {
				t.Fatal("partial/unproven resource task adopted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.vaultSyncs != syncs || f.launches != launches {
				t.Fatal("native recovery caused credential delivery/workload")
			}
		})
	}
}
