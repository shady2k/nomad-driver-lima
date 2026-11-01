package driver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func purgeFixture(t *testing.T) (FailedCreateOptions, *Store, *State, purgeDeps, *int) {
	o, store, s, old := failedRecoveryFixture(t)
	o.Instance = s.Instance
	s.Phase = "destroyed_disk_retained"
	s.Destroyed = true
	s.ConfigHash = "original-config"
	store.write(s)
	observation := purgeObservation{ConfigHash: s.ConfigHash, DirDev: 1, DirIno: 2}
	deleted := false
	calls := new(int)
	deps := purgeDeps{allocation: old.allocation, now: old.now, observe: func(context.Context, *State) (purgeObservation, error) {
		if deleted {
			return purgeObservation{Absent: true}, nil
		}
		return observation, nil
	}, remove: func(_ context.Context, name string) error {
		if name != s.Instance {
			t.Fatal("wrong deletion target")
		}
		*calls++
		deleted = true
		return nil
	}}
	return o, store, s, deps, calls
}
func TestPurgeRetainedAuditQuotaAndIdempotence(t *testing.T) {
	o, store, s, d, calls := purgeFixture(t)
	driver := &Driver{ctx: t.Context(), client: &lima.Client{Runner: newFake()}, store: store, config: Config{MaxTotalDiskGiB: 20}}
	if driver.checkBudget(TaskConfig{DiskGiB: 1}) == nil {
		t.Fatal("retained disk did not consume quota")
	}
	r, e := purgeRetained(t.Context(), o, d)
	if e != nil || r.Phase != "destroyed_disk_purged" || *calls != 1 {
		t.Fatal(r, e, *calls)
	}
	after, _ := store.read(s.Key)
	if after.DiskGiB != 0 || after.PurgedDiskGiB != 20 || after.Instance != s.Instance || !after.Destroyed {
		t.Fatal("original resource declaration lost")
	}
	if e = driver.checkBudget(TaskConfig{DiskGiB: 20}); e != nil {
		t.Fatal("confirmed purge did not release quota", e)
	}
	again, e := purgeRetained(t.Context(), o, d)
	if e != nil || !again.AlreadyRecovered || again.AuditSHA256 != r.AuditSHA256 || *calls != 1 {
		t.Fatal("not idempotent", again, e)
	}
	os.Remove(filepath.Join(store.Root, s.Key+".purge-complete.audit"))
	if driver.checkBudget(TaskConfig{DiskGiB: 1}) == nil {
		t.Fatal("missing audit silently released quota")
	}
}
func TestPurgeRejectsUnsafeBeforeDelete(t *testing.T) {
	for _, kind := range []string{"wrong-instance", "wrong-owner", "active-allocation", "changed-allocation", "next-allocation", "active-process", "symlink", "changed-config", "replaced-directory", "not-destroyed", "wrong-phase", "absent-no-intent", "live-lock", "admission-lock", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			o, store, s, d, calls := purgeFixture(t)
			read := d.allocation
			observe := d.observe
			n := 0
			d.allocation = func(c context.Context) (failedAllocation, error) {
				a, e := read(c)
				n++
				switch kind {
				case "active-allocation":
					a.ClientStatus = "running"
				case "changed-allocation":
					a.ModifyIndex += uint64(n)
				case "next-allocation":
					a.NextAllocation = "next"
				}
				return a, e
			}
			seen := 0
			d.observe = func(c context.Context, s *State) (purgeObservation, error) {
				seen++
				v, e := observe(c, s)
				switch kind {
				case "active-process", "symlink", "wrong-owner":
					return v, errors.New("ownership/absence unproven")
				case "changed-config":
					if seen > 1 {
						v.ConfigHash = "other"
					}
				case "replaced-directory":
					if seen > 1 {
						v.DirIno++
					}
				case "absent-no-intent":
					v = purgeObservation{Absent: true}
				}
				return v, e
			}
			switch kind {
			case "wrong-instance":
				o.Instance = "other-aaaaaaaaaaaa"
			case "not-destroyed":
				s.Destroyed = false
				store.write(s)
			case "wrong-phase":
				s.Phase = "running"
				store.write(s)
			}
			if kind == "live-lock" || kind == "admission-lock" {
				key := s.Key
				if kind == "admission-lock" {
					key = lima.Hash([]byte("node-admission"))
				}
				lock, e := store.lock(key)
				if e != nil {
					t.Fatal(e)
				}
				defer unlock(lock)
			}
			ctx := t.Context()
			if kind == "canceled" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if _, e := purgeRetained(ctx, o, d); e == nil {
				t.Fatal("unsafe purge accepted")
			}
			if *calls != 0 {
				t.Fatal("delete dispatched before proof")
			}
		})
	}
}
func TestPurgeUnknownDeleteAndPartialFailuresRetainQuota(t *testing.T) {
	for _, outcome := range []string{"no-effect", "actually-deleted", "partial-layout"} {
		t.Run(outcome, func(t *testing.T) {
			o, store, s, d, calls := purgeFixture(t)
			observe := d.observe
			deleted := false
			partial := false
			d.remove = func(context.Context, string) error {
				*calls++
				deleted = outcome == "actually-deleted"
				partial = outcome == "partial-layout"
				return errors.New("lost response")
			}
			d.observe = func(c context.Context, s *State) (purgeObservation, error) {
				if deleted {
					return purgeObservation{Absent: true}, nil
				}
				if partial {
					return purgeObservation{}, errors.New("config absent in partial directory")
				}
				return observe(c, s)
			}
			if _, e := purgeRetained(t.Context(), o, d); e == nil {
				t.Fatal("unknown delete claimed success")
			}
			pending, _ := store.read(s.Key)
			if pending.Phase != "purge_pending" || pending.PurgeIntentSHA256 == "" {
				t.Fatal(pending)
			}
			driver := &Driver{ctx: t.Context(), client: &lima.Client{Runner: newFake()}, store: store, config: Config{MaxTotalDiskGiB: 20}}
			if driver.checkBudget(TaskConfig{DiskGiB: 1}) == nil {
				t.Fatal("unknown deletion released quota")
			}
			if partial {
				if _, e := purgeRetained(t.Context(), o, d); e == nil {
					t.Fatal("partial layout retried")
				}
				if *calls != 1 {
					t.Fatal("blind retry")
				}
				return
			}
			d.remove = func(context.Context, string) error { *calls++; deleted = true; return nil }
			result, e := purgeRetained(t.Context(), o, d)
			if outcome == "no-effect" {
				if e == nil || *calls != 1 {
					t.Fatal("replayed ambiguous delete", result, e, *calls)
				}
				return
			}
			if e != nil || result.Phase != "destroyed_disk_purged" {
				t.Fatal(result, e)
			}
			want := 1
			if outcome == "actually-deleted" {
				want = 1
			}
			if *calls != want {
				t.Fatal("delete replay count", *calls, want)
			}
		})
	}
}
func TestLinuxPurgeUsesExactStoppedDeleteAndPreservesBase(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("disposable Linux root fixture only")
	}
	for _, kind := range []string{"success", "symlink", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			root, e := os.MkdirTemp("/tmp", "purge-fixture-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(root)
			os.Chmod(root, 0755)
			o, store, s, d, _ := purgeFixture(t)
			o.RuntimeUser = "nobody"
			o.LimaHome = filepath.Join(root, "home")
			os.Mkdir(o.LimaHome, 0700)
			os.Chown(o.LimaHome, 65534, 65534)
			instance := filepath.Join(o.LimaHome, s.Instance)
			os.Mkdir(instance, 0700)
			os.Chown(instance, 65534, 65534)
			config := []byte("owned synthetic config\n")
			os.WriteFile(filepath.Join(instance, "lima.yaml"), config, 0600)
			os.Chown(filepath.Join(instance, "lima.yaml"), 65534, 65534)
			s.ConfigHash = lima.Hash(config)
			store.write(s)
			base := filepath.Join(o.LimaHome, "base.img")
			os.WriteFile(base, []byte("immutable base"), 0444)
			info := lima.Info{Name: s.Instance, Status: "Stopped", VMType: "qemu", Dir: instance, CPUs: s.CPUs, Memory: int64(s.MemoryMiB) * 1048576, Disk: int64(s.DiskGiB) * 1073741824, Param: map[string]string{"nomad_driver_id": s.Owner, "nomad_task_key": s.Key}}
			if kind == "wrong-owner" {
				info.Param["nomad_driver_id"] = "other"
			}
			if kind == "symlink" {
				os.RemoveAll(instance)
				os.Symlink(base, instance)
			}
			encoded, _ := json.Marshal(info)
			name, _ := json.Marshal(s.Instance)
			body := "#!/usr/bin/python3\nimport os,sys,json,shutil,pathlib\nhome=pathlib.Path(os.environ['LIMA_HOME']);name=" + string(name) + "\nif sys.argv[1:]==['list','--json']:\n if (home/name).exists():print(" + string(mustJSON(string(encoded))) + ")\nelif sys.argv[1:]==['delete','--tty=false',name]:\n (home/'delete-call').write_text(json.dumps(sys.argv[1:]));shutil.rmtree(home/name)\nelse:raise SystemExit(44)\n"
			o.LimaBinary = filepath.Join(root, "limactl")
			os.WriteFile(o.LimaBinary, []byte(body), 0755)
			a, _ := d.allocation(t.Context())
			raw, _ := json.Marshal(a)
			o.NomadBinary = filepath.Join(root, "nomad")
			os.WriteFile(o.NomadBinary, []byte("#!/bin/sh\ncat <<'JSON'\n"+string(raw)+"\nJSON\n"), 0755)
			result, e := PurgeRetained(t.Context(), o)
			if kind == "success" {
				if e != nil || result.Phase != "destroyed_disk_purged" {
					t.Fatal(result, e)
				}
				b, _ := os.ReadFile(filepath.Join(o.LimaHome, "delete-call"))
				if string(b) != "[\"delete\", \"--tty=false\", \""+s.Instance+"\"]" {
					t.Fatal("wrong command", string(b))
				}
			} else {
				if e == nil {
					t.Fatal("unsafe live layout accepted")
				}
				if _, er := os.Stat(filepath.Join(o.LimaHome, "delete-call")); !os.IsNotExist(er) {
					t.Fatal("delete invoked")
				}
			}
			b, _ := os.ReadFile(base)
			if string(b) != "immutable base" {
				t.Fatal("base changed")
			}
		})
	}
}
func mustJSON(s string) []byte { b, _ := json.Marshal(s); return b }

func TestPurgeGC404RequiresExactLedgerAndFreshEmptyNode(t *testing.T) {
	for _, kind := range []string{"success", "missing-approval", "not-management", "changed-ledger", "forbidden", "node-forbidden", "wrong-node", "active-node", "node-unavailable", "changed-http-status"} {
		t.Run(kind, func(t *testing.T) {
			o, store, _, d, calls := purgeFixture(t)
			o.JobID = ""
			o.AllowGC404 = true
			original, _ := privateRecoveryRead(filepath.Join(store.Root, o.Key+".json"), 1<<20)
			o.ExpectedLedgerSHA256 = lima.Hash(original)
			n := 0
			d.allocation = func(context.Context) (failedAllocation, error) {
				n++
				a := failedAllocation{OperatorManagementVerified: true, HTTPStatus: 404, NodeHTTPStatus: 200, NodeAllocationsHTTPStatus: 200, VerifiedNodeID: o.NodeID}
				switch kind {
				case "forbidden":
					a.HTTPStatus = 403
				case "not-management":
					a.OperatorManagementVerified = false
				case "node-forbidden":
					a.NodeAllocationsHTTPStatus = 403
				case "wrong-node":
					a.VerifiedNodeID = "other"
				case "active-node":
					a.NodeActiveAllocations = 1
				case "node-unavailable":
					a.NodeHTTPStatus = 404
				case "changed-http-status":
					if n > 1 {
						a.HTTPStatus = 200
					}
				}
				return a, nil
			}
			switch kind {
			case "missing-approval":
				o.ExpectedLedgerSHA256 = ""
			case "changed-ledger":
				o.ExpectedLedgerSHA256 = lima.Hash([]byte("other"))
			}
			result, e := purgeRetained(t.Context(), o, d)
			if kind == "success" {
				if e != nil || *calls != 1 || result.Phase != "destroyed_disk_purged" {
					t.Fatal(result, e, *calls)
				}
				again, e := purgeRetained(t.Context(), o, d)
				if e != nil || !again.AlreadyRecovered || *calls != 1 {
					t.Fatal(again, e)
				}
			} else {
				if e == nil || *calls != 0 {
					t.Fatal("GC404 unproved purge", result, e, *calls)
				}
			}
		})
	}
}

func TestPurgeGCStdioBindsApprovedInstanceAndLedger(t *testing.T) {
	for _, kind := range []string{"success", "replayed-target"} {
		t.Run(kind, func(t *testing.T) {
			o, store, _, d, calls := purgeFixture(t)
			o.JobID = ""
			o.AllowGC404 = true
			original, _ := privateRecoveryRead(filepath.Join(store.Root, o.Key+".json"), 1<<20)
			o.ExpectedLedgerSHA256 = lima.Hash(original)
			input, controller := io.Pipe()
			requests, output := io.Pipe()
			defer input.Close()
			defer controller.Close()
			defer requests.Close()
			defer output.Close()
			verifier := newStdioRecoveryVerifier(o, input, output)
			d.allocation = verifier.read
			done := make(chan error, 1)
			go func() { _, e := purgeRetained(t.Context(), o, d); done <- e }()
			decoder := json.NewDecoder(requests)
			for seq := 1; seq <= 2; seq++ {
				var request recoveryChallenge
				if e := decoder.Decode(&request); e != nil {
					break
				}
				if request.Target.Instance != o.Instance || request.Target.LedgerSHA256 != o.ExpectedLedgerSHA256 || !request.Target.AllowGC404 {
					t.Fatal("approved identity not bound")
				}
				fact := failedAllocation{OperatorManagementVerified: true, HTTPStatus: 404, NodeHTTPStatus: 200, NodeAllocationsHTTPStatus: 200, VerifiedNodeID: o.NodeID}
				reply := recoveryObservation{1, "allocation_observation", seq, request.Nonce, request.Target, time.Now(), fact}
				if kind == "replayed-target" {
					reply.Target.Instance = "other-aaaaaaaaaaaa"
				}
				json.NewEncoder(controller).Encode(reply)
				if kind != "success" {
					break
				}
			}
			select {
			case e := <-done:
				if kind == "success" {
					if e != nil || *calls != 1 {
						t.Fatal(e, *calls)
					}
				} else if e == nil || *calls != 0 {
					t.Fatal("mismatched target deleted", e)
				}
			case <-time.After(time.Second):
				t.Fatal("stdio purge did not finish")
			}
		})
	}
}
