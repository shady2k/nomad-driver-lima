package driver

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/nomad/plugins/drivers"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type copyCLI struct {
	mu           sync.Mutex
	prepared     bool
	base         *fakeCLI
	order        []string
	failClean    bool
	sameIdentity bool
}

func (f *copyCLI) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	var p map[string]any
	_ = json.Unmarshal(in, &p)
	action, _ := p["action"].(string)
	if args[0] == "start" || args[0] == "stop" {
		f.mu.Lock()
		f.order = append(f.order, args[0])
		f.mu.Unlock()
	}
	if p["copy_sha256"] != nil {
		f.mu.Lock()
		f.order = append(f.order, "cleanup")
		if action == "clean" {
			f.prepared = true
		}
		prepared := f.prepared
		f.mu.Unlock()
		if !prepared {
			return nil, fmt.Errorf("proof absent; no replay")
		}
		if f.failClean {
			return nil, fmt.Errorf("synthetic cleanup response lost")
		}
		binding := map[string]any{}
		for k, v := range p {
			if k != "action" {
				binding[k] = v
			}
		}
		f.base.mu.Lock()
		n := f.base.starts
		f.base.mu.Unlock()
		return json.Marshal(map[string]any{"binding": binding, "known_runtime_credentials_removed": true, "reserved_runtime_root_removed": true, "workspace_preserved": true, "identity_reset_pending": true, "machine_id": fmt.Sprint("private-machine-", n), "boot_id": fmt.Sprint("boot-", n)})
	}
	if action == "sync" || action == "launch" {
		f.mu.Lock()
		f.order = append(f.order, action)
		f.mu.Unlock()
	}
	out, e := f.base.Run(ctx, in, args...)
	if args[0] == "shell" && e == nil {
		var v map[string]any
		if json.Unmarshal(out, &v) == nil && v["machine_id"] != nil {
			f.base.mu.Lock()
			n := f.base.starts
			f.base.mu.Unlock()
			if f.sameIdentity {
				n = 2
			}
			v["machine_id"] = fmt.Sprint("private-machine-", n)
			out, _ = json.Marshal(v)
		}
	}
	return out, e
}
func privateCopyFixture(t *testing.T) (*Driver, *fakeCLI, *copyCLI, *drivers.TaskConfig, *State, PrivateCopyInput) {
	t.Helper()
	f := newFake()
	d := setup(t, f, "")
	d.config.VaultAddress = "https://secrets.example.invalid"
	d.config.MaxTotalDiskGiB = 100
	cacheDir := filepath.Join(d.config.LimaHome, "_catalog-cache")
	if e := os.Mkdir(cacheDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Chown(cacheDir, os.Geteuid(), d.runtimeIdentity.gid); e != nil {
		t.Fatal(e)
	}
	old, token := tokenFixture(t)
	var tc TaskConfig
	old.DecodeDriverConfig(&tc)
	tc.CreatorAgentID = "00000000-0000-4000-8000-000000000004"
	tc.CreatorSessionID = "native-source"
	tc.Purpose = "prepare"
	old.EncodeConcreteDriverConfig(tc)
	os.WriteFile(token, []byte("synthetic-old"), 0600)
	if _, _, e := d.StartTask(old); e != nil {
		t.Fatal(e)
	}
	if e := d.StopTask(old.ID, time.Second, ""); e != nil {
		t.Fatal(e)
	}
	if e := d.DestroyTask(old.ID, false); e != nil {
		t.Fatal(e)
	}
	source, e := d.store.read(lima.Hash([]byte(old.ID)))
	if e != nil {
		t.Fatal(e)
	}
	// Actual legacy schema shape: do not manufacture the missing new resume fields.
	source.StopReceipt = nil
	source.NodeID = ""
	source.Namespace = ""
	source.Generation = 0
	source.VaultTokenSHA256 = ""
	if e = d.store.write(source); e != nil {
		t.Fatal(e)
	}
	raw := make([]byte, 4096)
	copy(raw, "QFI\xfb")
	binary.BigEndian.PutUint32(raw[4:8], 3)
	binary.BigEndian.PutUint64(raw[24:32], 1<<30)
	original := filepath.Join(d.config.LimaHome, source.Instance, "disk")
	os.WriteFile(original, raw, 0600)
	copyPath := filepath.Join(t.TempDir(), "disk.qcow2")
	os.WriteFile(copyPath, raw, 0600)
	input := PrivateCopyInput{ID: "restore-fixture", NodeID: "00000000-0000-4000-8000-000000000008", Namespace: "workers", CreatorAgentID: tc.CreatorAgentID, CreatorSessionID: tc.CreatorSessionID, CreatorGeneration: "00000000-0000-4000-8000-000000000006", SourceKey: source.Key, SourceAllocation: source.AllocID, SourceSession: source.WorkerSessionID, SourceInstance: source.Instance, CopyPath: copyPath, CopySHA256: lima.Hash(raw), CopySizeBytes: int64(len(raw)), VirtualSizeBytes: 1 << 30, Architecture: runtime.GOARCH, Workspace: "/workspace/repo", GuestStateDir: "/var/tmp/nomad-workers", Approved: true}
	manifest := PrivateCopyManifest{Version: 1, MaxReservedDiskGiB: 88, Inputs: map[string]PrivateCopyInput{input.ID: input}}
	body, _ := json.Marshal(manifest)
	path := filepath.Join(t.TempDir(), "inputs.config")
	os.WriteFile(path, body, 0600)
	d.config.PrivateCopiesFile = path
	d.config.PrivateCopiesSHA256 = lima.Hash(body)
	next, newToken := tokenFixture(t)
	next.ID = "private-copy/task/attempt"
	next.AllocID = "00000000-0000-4000-8000-000000000009"
	next.NodeID = input.NodeID
	next.Namespace = input.Namespace
	next.DecodeDriverConfig(&tc)
	tc.CreatorAgentID = input.CreatorAgentID
	tc.CreatorSessionID = input.CreatorSessionID
	tc.Purpose = "restore"
	tc.PrivateCopyID = input.ID
	tc.PrivateCopyOwnerGeneration = input.CreatorGeneration
	tc.WorkerSessionID = "00000000-0000-4000-8000-000000000005"
	next.EncodeConcreteDriverConfig(tc)
	os.WriteFile(newToken, []byte("synthetic-new-task"), 0600)
	f.mu.Lock()
	f.phase = "running"
	f.mu.Unlock()
	r := &copyCLI{base: f}
	d.client.Runner = r
	return d, f, r, next, source, input
}
func TestPrivateCopyLegacyOriginalUntouchedFreshIdentityBeforeToken(t *testing.T) {
	d, f, r, cfg, source, input := privateCopyFixture(t)
	oldPath := filepath.Join(d.store.Root, source.Key+".json")
	before, _ := os.ReadFile(oldPath)
	f.mu.Lock()
	syncs := f.vaultSyncs
	f.mu.Unlock()
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	current, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || current.Version != 3 || current.PrivateCopy.Phase != "ready" || current.Instance == source.Instance || current.MachineID == source.MachineID {
		t.Fatal("copy admission/identity missing", e)
	}
	after, _ := os.ReadFile(oldPath)
	if string(before) != string(after) {
		t.Fatal("original legacy ledger rewritten")
	}
	hash, _, _ := privateDigest(input.CopyPath)
	if hash != input.CopySHA256 {
		t.Fatal("input disk changed")
	}
	r.mu.Lock()
	order := strings.Join(r.order, ",")
	r.mu.Unlock()
	if !strings.Contains(order, "cleanup,stop,start,sync,launch") {
		t.Fatal("credentials before cleanup/reboot", order)
	}
	f.mu.Lock()
	currentSyncs := f.vaultSyncs
	f.mu.Unlock()
	if currentSyncs <= syncs || f.creates != 2 {
		t.Fatalf("unexpected extra create/token syncs=%d before=%d creates=%d", f.vaultSyncs, syncs, f.creates)
	}
	if _, _, e = d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	if f.creates != 2 {
		t.Fatal("replayed import")
	}
	// New control config changes cannot silently replace this durable input snapshot.
	d.config.PrivateCopiesFile = ""
	d.config.PrivateCopiesSHA256 = ""
	if _, _, e = d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
}
func TestPrivateCopyFailuresNeverDeliverNewTokenOrTouchOriginal(t *testing.T) {
	for _, kind := range []string{"owner", "hash", "active-source", "cleanup", "same-identity", "cap"} {
		t.Run(kind, func(t *testing.T) {
			d, f, r, cfg, source, input := privateCopyFixture(t)
			f.mu.Lock()
			syncs := f.vaultSyncs
			f.mu.Unlock()
			creates := f.creates
			switch kind {
			case "owner":
				var tc TaskConfig
				cfg.DecodeDriverConfig(&tc)
				tc.PrivateCopyOwnerGeneration = "00000000-0000-4000-8000-000000000099"
				cfg.EncodeConcreteDriverConfig(tc)
			case "hash":
				os.WriteFile(input.CopyPath, []byte("changed"), 0600)
			case "active-source":
				f.mu.Lock()
				info := f.infos[source.Instance]
				info.Status = "Running"
				f.infos[source.Instance] = info
				f.mu.Unlock()
			case "cleanup":
				r.failClean = true
			case "same-identity":
				r.sameIdentity = true
			case "cap":
				d.config.MaxTotalDiskGiB = 1
			}
			if _, _, e := d.StartTask(cfg); e == nil {
				t.Fatal("unsafe copy admitted")
			}
			f.mu.Lock()
			currentSyncs := f.vaultSyncs
			f.mu.Unlock()
			if currentSyncs != syncs {
				t.Fatal("token delivered before safe preparation")
			}
			if kind == "owner" || kind == "hash" || kind == "active-source" || kind == "cap" {
				if f.creates != creates {
					t.Fatal("created before admission")
				}
			}
			old, e := d.store.read(source.Key)
			if e != nil || old.StopReceipt != nil || old.ResumeNext != nil {
				t.Fatal("legacy proof fabricated", e)
			}
		})
	}
}

func TestPrivateCopyCleanupUnknownProbesWithoutReplay(t *testing.T) {
	d, f, r, cfg, _, _ := privateCopyFixture(t)
	r.failClean = true
	if _, _, e := d.StartTask(cfg); e == nil {
		t.Fatal("lost response accepted")
	}
	s, e := d.store.read(lima.Hash([]byte(cfg.ID)))
	if e != nil || s.PrivateCopy.Phase != "cleanup_dispatched" {
		t.Fatal("missing uncertain checkpoint", e)
	}
	f.mu.Lock()
	creates := f.creates
	syncs := f.vaultSyncs
	f.mu.Unlock()
	r.failClean = false
	if _, _, e = d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	currentCreates, currentSyncs := f.creates, f.vaultSyncs
	f.mu.Unlock()
	if currentCreates != creates || currentSyncs <= syncs {
		t.Fatal("duplicate create or missing post-proof token")
	}
}
func TestPrivateCopyNoSecondAllocationClaimsSameInput(t *testing.T) {
	d, _, _, cfg, _, _ := privateCopyFixture(t)
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
	cfg.ID = "second-allocation/task/attempt"
	cfg.AllocID = "00000000-0000-4000-8000-000000000099"
	if _, _, e := d.StartTask(cfg); e == nil || !strings.Contains(e.Error(), "already claimed") {
		t.Fatal("duplicate input claim accepted", e)
	}
}
func TestPrivateCopyBudgetMatchesNormalNoVMAccounting(t *testing.T) {
	d, _, _, cfg, _, _ := privateCopyFixture(t)
	s := &State{Version: 1, Key: lima.Hash([]byte("no-create")), Owner: d.store.Owner, TaskID: "no-create", Phase: "no_vm_failed", DiskGiB: 1000, Destroyed: true}
	if e := d.store.write(s); e != nil {
		t.Fatal(e)
	}
	if _, _, e := d.StartTask(cfg); e != nil {
		t.Fatal(e)
	}
}
