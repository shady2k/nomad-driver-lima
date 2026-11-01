package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
	"io"
	"nomad-driver-lima/internal/catalog"
	"nomad-driver-lima/internal/lima"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

const Version = "0.1.0-prototype"

type Config struct {
	VaultAddress      string `codec:"vault_address"`
	CatalogFile       string `codec:"catalog_file"`
	CatalogSHA256     string `codec:"catalog_sha256"`
	MaxVMs            int    `codec:"max_vms"`
	MaxTotalCPUs      int    `codec:"max_total_vcpus"`
	MaxTotalMemoryMiB int    `codec:"max_total_memory_mib"`
	MaxTotalDiskGiB   int    `codec:"max_total_disk_gib"`
	MaxGuestCPUs      int    `codec:"max_guest_vcpus"`
	MaxGuestMemoryMiB int    `codec:"max_guest_memory_mib"`
	MaxGuestDiskGiB   int    `codec:"max_guest_disk_gib"`
	LimaBinary        string `codec:"limactl"`
	LimaHome          string `codec:"lima_home"`
	StateDir          string `codec:"state_dir"`
	TemplateFile      string `codec:"template_file"`
	TemplateSHA256    string `codec:"template_sha256"`
	ExpectedVersion   string `codec:"expected_version"`
	MemoryOverheadMiB int    `codec:"memory_overhead_mib"`
	DiskFreeFloorMiB  int    `codec:"disk_free_floor_mib"`
	RuntimeUser       string `codec:"runtime_user"`
}
type TaskConfig struct {
	VaultRequired         bool     `codec:"vault_required"`
	WorkerSessionID       string   `codec:"worker_session_id"`
	TemplateID            string   `codec:"template_id"`
	TemplateVersion       string   `codec:"template_version"`
	TemplateDiskSHA256    string   `codec:"template_disk_sha256"`
	TemplateCatalogSHA256 string   `codec:"template_catalog_sha256"`
	TemplateVariantsJSON  string   `codec:"template_variants_json"`
	CreatorAgentID        string   `codec:"creator_agent_id"`
	CreatorSessionID      string   `codec:"creator_session_id"`
	Project               string   `codec:"project"`
	Purpose               string   `codec:"purpose"`
	TaskLink              string   `codec:"task_link"`
	Command               []string `codec:"command"`
	CPUs                  int      `codec:"guest_cpus"`
	MemoryMiB             int      `codec:"memory_mib"`
	DiskGiB               int      `codec:"disk_gib"`
	Architecture          string   `codec:"architecture"`
	GuestOS               string   `codec:"guest_os"`
	resolvedYAMLSHA256    string
	resolvedVMType        string
}
type task struct {
	vaultMu         sync.Mutex
	vaultStopped    bool
	vaultHash       string
	lifecycleBuffer []byte
	reportSignature string
	mu              sync.Mutex
	state           *State
	cfg             *drivers.TaskConfig
	done            chan struct{}
	once            sync.Once
	lock            *os.File
	out, err        io.WriteCloser
	lastErr         error
}
type Driver struct {
	runtimeIdentity *runtimeIdentity
	reconcileOnce   sync.Once
	configMu        sync.RWMutex
	configInput     *Config
	eventMu         sync.Mutex
	eventHistory    []*drivers.TaskEvent
	subscribers     map[chan *drivers.TaskEvent]bool
	ctx             context.Context
	cancel          context.CancelFunc
	logger          log.Logger
	mu              sync.Mutex
	tasks           map[string]*task
	client          *lima.Client
	config          Config
	store           *Store
	mhzPerCore      int
	wg              sync.WaitGroup
	interval        time.Duration
}

var _ drivers.DriverPlugin = (*Driver)(nil)

func New(ctx context.Context, l log.Logger) *Driver {
	ctx, cancel := context.WithCancel(ctx)
	return &Driver{ctx: ctx, cancel: cancel, logger: l, tasks: map[string]*task{}, interval: time.Second}
}
func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return &base.PluginInfoResponse{Type: base.PluginTypeDriver, PluginApiVersions: []string{"0.1.0"}, PluginVersion: Version, Name: "lima"}, nil
}
func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	m := map[string]*hclspec.Spec{}
	for _, k := range []string{"limactl", "lima_home", "state_dir", "expected_version"} {
		m[k] = hclspec.NewAttr(k, "string", true)
	}
	for _, k := range []string{"template_file", "template_sha256", "catalog_file", "catalog_sha256"} {
		m[k] = hclspec.NewAttr(k, "string", false)
	}
	for _, k := range []string{"max_vms", "max_total_vcpus", "max_total_memory_mib", "max_total_disk_gib", "max_guest_vcpus", "max_guest_memory_mib", "max_guest_disk_gib"} {
		m[k] = hclspec.NewAttr(k, "number", false)
	}
	m["vault_address"] = hclspec.NewAttr("vault_address", "string", false)
	m["runtime_user"] = hclspec.NewAttr("runtime_user", "string", false)
	for _, k := range []string{"memory_overhead_mib", "disk_free_floor_mib"} {
		m[k] = hclspec.NewAttr(k, "number", true)
	}
	return hclspec.NewObject(m), nil
}
func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	m := map[string]*hclspec.Spec{"command": hclspec.NewAttr("command", "list(string)", true)}
	for _, k := range []string{"guest_cpus", "memory_mib", "disk_gib"} {
		m[k] = hclspec.NewAttr(k, "number", true)
	}
	for _, k := range []string{"guest_os"} {
		m[k] = hclspec.NewAttr(k, "string", true)
	}
	for _, k := range []string{"architecture", "template_catalog_sha256", "template_variants_json", "worker_session_id", "template_id", "template_version", "template_disk_sha256", "creator_agent_id", "creator_session_id", "project", "purpose", "task_link"} {
		m[k] = hclspec.NewAttr(k, "string", false)
	}
	m["vault_required"] = hclspec.NewAttr("vault_required", "bool", false)
	return hclspec.NewObject(m), nil
}

// Nomad validates plugin configuration before it has scanned CPU topology.
// SDK serialization may turn a nil AgentConfig into a non-nil empty one.
func (d *Driver) SetConfig(c *base.Config) error {
	if c == nil {
		return fmt.Errorf("nil plugin configuration")
	}
	var cfg Config
	if e := base.MsgPackDecode(c.PluginConfig, &cfg); e != nil {
		return e
	}
	v := c.AgentConfig.Compute()
	perCore := 0
	if v.TotalCompute > 0 && v.NumCores > 0 {
		var e error
		perCore, e = nomadComputePerCore(uint64(v.TotalCompute), uint64(v.NumCores))
		if e != nil {
			return e
		}
	}
	d.configMu.Lock()
	defer d.configMu.Unlock()
	if d.client == nil {
		if e := d.configure(cfg); e != nil {
			return e
		}
		saved := cfg
		d.configInput = &saved
	} else if d.configInput == nil || cfg != *d.configInput {
		return fmt.Errorf("runtime config change requires a new driver process")
	}
	// Zero is pending/unknown, never fabricated schedulable capacity. A later
	// SetConfig from the driver manager supplies the real topology.
	d.mhzPerCore = perCore
	return nil
}
func (d *Driver) admissionCompute() (int, int) {
	d.configMu.RLock()
	defer d.configMu.RUnlock()
	return d.mhzPerCore, d.config.MemoryOverheadMiB
}
func (d *Driver) configure(cfg Config) error {
	identity, e := resolveRuntimeUser(cfg.RuntimeUser)
	if e != nil {
		return e
	}
	if !regexp.MustCompile(`^2\.[0-9]+\.[0-9]+$`).MatchString(cfg.ExpectedVersion) {
		return fmt.Errorf("explicit stable Lima2.x version required; pre-release/nightly unsupported")
	}
	if cfg.MemoryOverheadMiB < 1 || cfg.DiskFreeFloorMiB < 0 {
		return fmt.Errorf("explicit positive memory overhead and nonnegative disk floor required")
	}
	for _, p := range []string{cfg.LimaBinary, cfg.LimaHome, cfg.StateDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("absolute operator paths required")
		}
	}
	if cfg.CatalogFile != "" {
		if !filepath.IsAbs(cfg.CatalogFile) {
			return fmt.Errorf("absolute catalog path required")
		}
		if _, e := catalog.Read(cfg.CatalogFile, cfg.CatalogSHA256); e != nil {
			return e
		}
	} else {
		if !filepath.IsAbs(cfg.TemplateFile) {
			return fmt.Errorf("absolute legacy template or approved catalog required")
		}
		b, e := os.ReadFile(cfg.TemplateFile)
		if e != nil {
			return e
		}
		if lima.Hash(b) != cfg.TemplateSHA256 {
			return fmt.Errorf("operator template checksum mismatch")
		}
	}
	for _, n := range []int{cfg.MaxVMs, cfg.MaxTotalCPUs, cfg.MaxTotalMemoryMiB, cfg.MaxTotalDiskGiB, cfg.MaxGuestCPUs, cfg.MaxGuestMemoryMiB, cfg.MaxGuestDiskGiB} {
		if n < 0 {
			return fmt.Errorf("negative operator budget")
		}
	}
	if e = os.MkdirAll(cfg.LimaHome, 0700); e != nil {
		return e
	}
	cfg.LimaHome, e = filepath.EvalSymlinks(cfg.LimaHome)
	if e != nil {
		return e
	}
	st, e := os.Stat(cfg.LimaHome)
	if e != nil || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("private Lima home required")
	}
	if e = identity.validateHome(cfg.LimaHome); e != nil {
		return e
	}
	if e = lima.ValidateHome(cfg.LimaHome); e != nil {
		return e
	}
	store, e := openStore(cfg.StateDir)
	if e != nil {
		return e
	}
	d.config = cfg
	d.runtimeIdentity = identity
	d.store = store
	d.client = &lima.Client{Home: cfg.LimaHome, Runner: lima.CommandRunner{Binary: cfg.LimaBinary, Home: cfg.LimaHome, UserHome: identity.home, Credential: identity.credential, Diagnostics: filepath.Join(cfg.StateDir, "diagnostics")}}
	d.reconcileOnce.Do(func() { d.wg.Add(1); go d.reconcileUnattached() })
	return nil
}
func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return &drivers.Capabilities{SendSignals: true, Exec: true, FSIsolation: fsisolation.Image, NetIsolationModes: []drivers.NetIsolationMode{drivers.NetIsolationModeHost}, MountConfigs: drivers.MountConfigSupportNone}, nil
}
func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go func() {
		defer close(ch)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			fp := &drivers.Fingerprint{Health: drivers.HealthStateUnhealthy, Attributes: map[string]*pstructs.Attribute{"driver.lima.guest.os": pstructs.NewStringAttribute("linux"), "driver.lima.guest.arch": pstructs.NewStringAttribute(runtime.GOARCH), "driver.lima.network_isolated": pstructs.NewBoolAttribute(false), "driver.lima.macos_guest": pstructs.NewBoolAttribute(false)}}
			d.configMu.RLock()
			client, expected, perCore := d.client, d.config.ExpectedVersion, d.mhzPerCore
			capacityConfig, capacityStore := d.config, d.store
			policy := map[string]int{"max_vms": d.config.MaxVMs, "max_total_vcpus": d.config.MaxTotalCPUs, "max_total_memory_mib": d.config.MaxTotalMemoryMiB, "max_total_disk_gib": d.config.MaxTotalDiskGiB, "max_guest_vcpus": d.config.MaxGuestCPUs, "max_guest_memory_mib": d.config.MaxGuestMemoryMiB, "max_guest_disk_gib": d.config.MaxGuestDiskGiB, "memory_overhead_mib": d.config.MemoryOverheadMiB}
			d.configMu.RUnlock()
			fp.Attributes["driver.lima.catalog.late_platform_selection"] = pstructs.NewBoolAttribute(true)
			fp.Attributes["driver.lima.catalog.sha256"] = pstructs.NewStringAttribute(capacityConfig.CatalogSHA256)
			for k, v := range policy {
				fp.Attributes["driver.lima.operator_limits."+k] = pstructs.NewIntAttribute(int64(v), "")
			}
			for k, v := range capacityAttributes(capacityConfig, capacityStore) {
				fp.Attributes[k] = v
			}
			for k, v := range metricAttributes(perCore) {
				fp.Attributes[k] = v
			}
			if perCore < 1 {
				fp.HealthDescription = "Nomad compute topology pending; task admission disabled"
			} else if client != nil {
				c, cancel := context.WithTimeout(ctx, 10*time.Second)
				v, e := client.Version(c)
				if e == nil && v == "limactl version "+expected {
					_, e = client.List(c)
				}
				cancel()
				if e == nil && v == "limactl version "+expected {
					fp.Health = drivers.HealthStateHealthy
					fp.HealthDescription = "official Lima CLI reachable; prototype, network policy not isolated"
					if policy["max_total_disk_gib"] < 1 {
						d.configMu.RLock()
						catalogMode := d.config.CatalogFile != ""
						d.configMu.RUnlock()
						if catalogMode {
							fp.Health = drivers.HealthStateUnhealthy
							fp.HealthDescription = "catalog disk budget pending; task admission disabled"
						}
					}
				} else {
					fp.HealthDescription = "Lima CLI unavailable/version mismatch"
				}
			}
			select {
			case ch <- fp:
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
func (d *Driver) emit(s *State, phase string) {
	e := &drivers.TaskEvent{TaskID: s.TaskID, TaskName: s.TaskName, AllocID: s.AllocID, Timestamp: time.Now(), Message: "lima " + phase, Annotations: selectionAnnotations(s, phase)}
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	d.eventHistory = append(d.eventHistory, e)
	if len(d.eventHistory) > 128 {
		d.eventHistory = d.eventHistory[len(d.eventHistory)-128:]
	}
	for c := range d.subscribers {
		select {
		case c <- e:
		default:
		}
	}
}
func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	ch := make(chan *drivers.TaskEvent, 256)
	d.eventMu.Lock()
	if d.subscribers == nil {
		d.subscribers = map[chan *drivers.TaskEvent]bool{}
	}
	for _, e := range d.eventHistory {
		ch <- e
	}
	d.subscribers[ch] = true
	d.eventMu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-d.ctx.Done():
		}
		d.eventMu.Lock()
		delete(d.subscribers, ch)
		close(ch)
		d.eventMu.Unlock()
	}()
	return ch, nil
}

func (d *Driver) validate(cfg *drivers.TaskConfig) (TaskConfig, string, error) {
	var tc TaskConfig
	if cfg == nil || cfg.ID == "" || cfg.AllocID == "" || cfg.Name == "" {
		return tc, "", fmt.Errorf("Nomad task identity required")
	}
	if e := cfg.DecodeDriverConfig(&tc); e != nil {
		return tc, "", e
	}
	if e := d.resolvePlatform(&tc); e != nil {
		return tc, "", e
	}
	if tc.GuestOS != "linux" || tc.Architecture != runtime.GOARCH {
		return tc, "", fmt.Errorf("only native architecture Linux guests supported")
	}
	if len(tc.Command) == 0 || tc.Command[0] == "" || tc.CPUs < 1 || tc.MemoryMiB < 256 || tc.DiskGiB < 1 || tc.DiskGiB > 1048576 {
		return tc, "", fmt.Errorf("invalid guest request")
	}
	if tc.VaultRequired && !validVaultAddress(d.config.VaultAddress) {
		return tc, "", fmt.Errorf("operator HTTPS vault_address required")
	}
	if tc.VaultRequired && (!vaultSessionID.MatchString(tc.WorkerSessionID) || cfg.Env["VAULT_TOKEN"] != "" || cfg.Env["BAO_TOKEN"] != "" || cfg.Env["LLM_API_KEY"] != "") {
		return tc, "", fmt.Errorf("vault requires task token file and canonical worker session; secret environment forbidden")
	}
	if cfg.User != "" || len(cfg.Mounts) > 0 {
		return tc, "", fmt.Errorf("host user/mount overrides unsupported")
	}
	perCore, overhead := d.admissionCompute()
	if cfg.Resources == nil || cfg.Resources.NomadResources == nil || perCore < 1 {
		return tc, "", fmt.Errorf("native Nomad compute/reservations unavailable")
	}
	nr := cfg.Resources.NomadResources
	if !reservationCovers(int64(tc.CPUs), int64(perCore), int64(tc.MemoryMiB), int64(overhead), nr.Cpu.CpuShares, nr.Memory.MemoryMB) {
		return tc, "", fmt.Errorf("Nomad reservation does not cover guest and overhead")
	}
	templateHash := d.config.TemplateSHA256
	if d.config.CatalogFile != "" {
		if d.config.MaxTotalDiskGiB < 1 {
			return tc, "", fmt.Errorf("catalog node disk budget pending; task admission disabled")
		}
		entry, e := d.catalogEntry(tc)
		if e != nil {
			return tc, "", e
		}
		templateHash = entry.Disk.SHA256 + ":" + entry.YAML.SHA256
		if e = validateCreator(tc); e != nil {
			return tc, "", e
		}
	}
	if exceeds(tc.CPUs, d.config.MaxGuestCPUs) || exceeds(tc.MemoryMiB, d.config.MaxGuestMemoryMiB) || exceeds(tc.DiskGiB, d.config.MaxGuestDiskGiB) {
		return tc, "", fmt.Errorf("guest request exceeds operator limit")
	}
	b, _ := json.Marshal(struct {
		Task     TaskConfig
		Template string
	}{tc, templateHash})
	return tc, lima.Hash(b), nil
}
func (d *Driver) owned(ctx context.Context, s *State) (*lima.Info, error) {
	v, e := d.client.Get(ctx, s.Instance)
	if e != nil {
		return nil, e
	}
	if v.Param["nomad_driver_id"] != s.Owner || v.Param["nomad_task_key"] != s.Key || v.CPUs != s.CPUs || v.Memory != int64(s.MemoryMiB)*1048576 || v.Disk != int64(s.DiskGiB)*1073741824 {
		return nil, fmt.Errorf("Lima ownership/resources mismatch; VM retained")
	}
	if filepath.Clean(v.Dir) != filepath.Join(d.config.LimaHome, s.Instance) {
		return nil, fmt.Errorf("Lima directory mismatch")
	}
	h, e := d.client.ConfigHash(s.Instance)
	if e != nil {
		return nil, e
	}
	if s.ConfigHash != "" && s.ConfigHash != h {
		return nil, fmt.Errorf("Lima configuration changed; VM retained")
	}
	return v, nil
}
func (d *Driver) payload(s *State, action string) map[string]any {
	return map[string]any{"key": s.Key, "owner": s.Owner, "action": action, "stdout_offset": s.OutOffset, "stderr_offset": s.ErrOffset}
}
func (d *Driver) handle(t *task) (*drivers.TaskHandle, error) {
	h := drivers.NewTaskHandle(1)
	h.Config = t.cfg
	e := h.SetDriverState(map[string]string{"key": t.state.Key, "owner": t.state.Owner, "spec": t.state.SpecHash})
	return h, e
}
func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	tc, hash, e := d.validate(cfg)
	if e != nil {
		return nil, nil, e
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.tasks[cfg.ID]; t != nil {
		if t.state.SpecHash != hash {
			return nil, nil, fmt.Errorf("task spec changed")
		}
		h, e := d.handle(t)
		return h, nil, e
	}
	// Nomad logmon reads until FIFO EOF. Keep a writer continuously open from
	// the first control-stage message until attach owns the guest stream writer.
	// Per-stage open/close alone would terminate logmon before VM boot finishes.
	logGuard, e := os.OpenFile(cfg.StdoutPath, os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return nil, nil, fmt.Errorf("native stdout unavailable before startup")
	}
	defer logGuard.Close()
	key := lima.Hash([]byte(cfg.ID))
	lock, e := d.store.lock(key)
	if e != nil {
		return nil, nil, e
	}
	accepted := false
	noVMConfirmed := false
	var failedState *State
	defer func() {
		if !accepted {
			if noVMConfirmed && failedState != nil && failedState.Phase == "intent" {
				failedState.Phase = "no_vm_failed"
				_ = d.store.write(failedState)
			}
			unlock(lock)
		}
	}()
	s, e := d.store.read(key)
	fresh := os.IsNotExist(e)
	if e != nil && !fresh {
		return nil, nil, e
	}
	if fresh {
		admission, e := d.store.lock(lima.Hash([]byte("node-admission")))
		if e != nil {
			return nil, nil, e
		}
		defer unlock(admission)
		if e = d.checkBudget(tc); e != nil {
			return nil, nil, e
		}
		s = &State{Version: 1, Key: key, Owner: d.store.Owner, TaskID: cfg.ID, AllocID: cfg.AllocID, TaskName: cfg.Name, Instance: instanceName(tc, key), SpecHash: hash, Phase: "intent", CPUs: tc.CPUs, MemoryMiB: tc.MemoryMiB, DiskGiB: tc.DiskGiB, CreatorAgentID: tc.CreatorAgentID, CreatorSessionID: tc.CreatorSessionID, WorkerSessionID: tc.WorkerSessionID, TemplateID: tc.TemplateID, TemplateVersion: tc.TemplateVersion, TemplateDiskSHA256: tc.TemplateDiskSHA256, TemplateCatalogSHA256: tc.TemplateCatalogSHA256, TemplateYAMLSHA256: tc.resolvedYAMLSHA256, ResolvedArchitecture: tc.Architecture, ResolvedVMType: tc.resolvedVMType, Purpose: tc.Purpose, Project: tc.Project, TaskLink: tc.TaskLink}
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
	} else if s.SpecHash != hash || s.TaskID != cfg.ID || s.Destroyed {
		return nil, nil, fmt.Errorf("existing task identity/spec mismatch")
	}
	if e = verifyResolvedState(tc, s); e != nil {
		return nil, nil, e
	}
	failedState = s
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Minute)
	defer cancel()
	if e = d.stage(cfg, s, "admission_verified", nil); e != nil {
		return nil, nil, e
	}

	v, e := d.client.Get(ctx, s.Instance)
	if errors.Is(e, lima.ErrNotFound) {
		noVMConfirmed = true
		if s.Phase != "intent" {
			if s.Phase == "no_vm_failed" {
				return nil, nil, fmt.Errorf("previous pre-create failure confirmed no VM; inspect logs and use a new allocation")
			}
			return nil, nil, fmt.Errorf("uncertain create: no retry, retain ledger")
		}
		if e = physicalFree(d.config.LimaHome, tc.DiskGiB, d.config.DiskFreeFloorMiB); e != nil {
			return nil, nil, e
		}
		templateFile := d.config.TemplateFile
		if d.config.CatalogFile != "" {
			entry, er := d.catalogEntry(tc)
			if er != nil {
				return nil, nil, er
			}
			cache := catalog.Cache{Dir: filepath.Join(d.config.LimaHome, "_catalog-cache"), Runtime: &catalog.Access{UID: d.runtimeIdentity.uid, GID: d.runtimeIdentity.gid}, Progress: func(phase string, n, total int64) {
				_ = d.stage(cfg, s, phase, map[string]any{"bytes": n, "total_bytes": total})
			}}
			templateFile, er = cache.Materialize(ctx, entry)
			if er != nil {
				_ = d.stage(cfg, s, "artifact_failed", nil)
				return nil, nil, er
			}
		}
		if e = d.client.ValidateTemplate(ctx, templateFile); e != nil {
			return nil, nil, fmt.Errorf("runtime template preflight failed before create: %w", e)
		}
		if e = d.stage(cfg, s, "vm_create", nil); e != nil {
			return nil, nil, e
		}
		s.Phase = "creating"
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
		noVMConfirmed = false // Once dispatched, an uncertain create must retain its reservation.
		e = d.client.Create(ctx, lima.Request{Name: s.Instance, Owner: s.Owner, Key: s.Key, Template: templateFile, CPUs: tc.CPUs, MemoryMiB: tc.MemoryMiB, DiskGiB: tc.DiskGiB})
		if e != nil {
			return nil, nil, e
		}
		v, e = d.client.Get(ctx, s.Instance)
	}
	if e != nil {
		return nil, nil, e
	}
	v, e = d.owned(ctx, s)
	if e != nil {
		return nil, nil, e
	}
	if s.ConfigHash == "" {
		s.ConfigHash, e = d.client.ConfigHash(s.Instance)
		if e != nil {
			return nil, nil, e
		}
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
	}
	if v.Status != "Running" {
		if s.MachineID != "" {
			return nil, nil, fmt.Errorf("previously started VM is not running; no reboot/replay")
		}
		s.Phase = "starting"
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
		if e = d.stage(cfg, s, "vm_boot", nil); e != nil {
			return nil, nil, e
		}
		if e = d.client.Start(ctx, s.Instance); e != nil {
			return nil, nil, e
		}
	}
	if s.MachineID == "" {
		_, id, e := d.client.Guest(ctx, s.Instance, d.payload(s, "identity"))
		if e != nil {
			return nil, nil, e
		}
		if id.MachineID == "" || id.BootID == "" {
			return nil, nil, fmt.Errorf("missing guest identity")
		}
		if e = d.uniqueIdentity(s, id.MachineID); e != nil {
			return nil, nil, e
		}
		if e = d.stage(cfg, s, "guest_identity_verified", nil); e != nil {
			return nil, nil, e
		}
		s.MachineID = id.MachineID
		s.BootID = id.BootID
		s.Phase = "launching"
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
		if e = d.stage(cfg, s, "workload_launch", nil); e != nil {
			return nil, nil, e
		}
		if _, e = d.syncVault(ctx, cfg, s, ""); e != nil {
			return nil, nil, e
		}
		p := d.payload(s, "launch")
		p["command"] = tc.Command
		if _, _, e = d.client.Guest(ctx, s.Instance, p); e != nil {
			return nil, nil, fmt.Errorf("uncertain launch; no replay: %w", e)
		}
		s.StartedAt = time.Now()
		s.Phase = "running"
		if e = d.store.write(s); e != nil {
			return nil, nil, e
		}
	}
	t, e := d.attach(cfg, s, lock)
	if e != nil {
		return nil, nil, e
	}
	d.tasks[cfg.ID] = t
	d.emit(s, "started")
	accepted = true
	h, e := d.handle(t)
	return h, nil, e
}
func (d *Driver) attach(cfg *drivers.TaskConfig, s *State, lock *os.File) (*task, error) {
	out, e := os.OpenFile(cfg.StdoutPath, os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return nil, e
	}
	errout, e := os.OpenFile(cfg.StderrPath, os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		out.Close()
		return nil, e
	}
	if s.Exit == nil && !s.VaultFinalizing {
		secretCtx, secretCancel := context.WithTimeout(d.ctx, 15*time.Second)
		_, e = d.syncVault(secretCtx, cfg, s, "")
		secretCancel()
	}
	if e != nil {
		out.Close()
		errout.Close()
		return nil, e
	}
	t := &task{vaultStopped: s.VaultFinalizing, cfg: cfg, state: s, done: make(chan struct{}), lock: lock, out: out, err: errout}
	d.wg.Add(1)
	go d.monitor(t)
	return t, nil
}
func (d *Driver) monitor(t *task) {
	defer d.wg.Done()
	defer unlock(t.lock)
	defer t.out.Close()
	defer t.err.Close()
	for {
		if d.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		t.mu.Lock()
		s := *t.state
		t.mu.Unlock()
		if s.Exit != nil {
			cancel()
			t.once.Do(func() { close(t.done) })
			return
		}
		v, e := d.owned(ctx, &s)
		if e == nil && v.Status != "Running" {
			e = fmt.Errorf("VM not running without confirmed guest result")
		}
		var snap lima.Snapshot
		var ident lima.Identity
		if e == nil {
			t.vaultMu.Lock()
			if !t.vaultStopped {
				t.vaultHash, e = d.syncVault(ctx, t.cfg, &s, t.vaultHash)
			}
			t.vaultMu.Unlock()
			if e == nil {
				snap, ident, e = d.client.Guest(ctx, s.Instance, d.payload(&s, "status"))
			}
		}
		cancel()
		if d.ctx.Err() != nil {
			return
		}
		if e != nil {
			t.mu.Lock()
			t.lastErr = e
			d.reportActivity(t, time.Now(), true)
			t.mu.Unlock()
		} else {
			if ident.MachineID != "" && (ident.MachineID != s.MachineID || ident.BootID != s.BootID) {
				e = fmt.Errorf("guest identity changed")
			}
			if snap.Phase == "unknown" {
				e = fmt.Errorf("guest workload state unknown; no replay")
			}
			if e == nil {
				_, e = t.out.Write(snap.Stdout)
				if e == nil {
					_, e = t.err.Write(snap.Stderr)
				}
			}
			t.mu.Lock()
			if e == nil {
				d.observeActivity(t, snap.Stdout, time.Now())
				t.state.OutOffset = snap.StdoutOffset
				t.state.ErrOffset = snap.StderrOffset
				t.lastErr = nil
				if snap.Result != nil && snap.StdoutEOF && snap.StderrEOF {
					t.state.Exit = snap.Result
					t.state.Phase = "exited"
					t.state.CompletedAt = time.Now()
				}
			} else {
				t.lastErr = e
			}
			d.reportActivity(t, time.Now(), t.lastErr != nil)
			_ = d.store.write(t.state)
			exited := t.state.Exit != nil
			t.mu.Unlock()
			if exited {
				d.emit(t.state, "guest_workload_exited")
				t.once.Do(func() { close(t.done) })
				return
			}
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(d.interval):
		}
	}
}
func (d *Driver) RecoverTask(h *drivers.TaskHandle) error {
	if h == nil || h.Config == nil {
		return fmt.Errorf("missing task handle")
	}
	var b map[string]string
	if e := h.GetDriverState(&b); e != nil {
		return e
	}
	if b["key"] != lima.Hash([]byte(h.Config.ID)) || b["owner"] != d.store.Owner {
		return fmt.Errorf("handle owner mismatch")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tasks[h.Config.ID] != nil {
		return nil
	}
	lock, e := d.store.lock(b["key"])
	if e != nil {
		return e
	}
	s, e := d.store.read(b["key"])
	if e != nil {
		unlock(lock)
		return e
	}
	if s.SpecHash != b["spec"] || s.TaskID != h.Config.ID || s.Destroyed {
		unlock(lock)
		return fmt.Errorf("recovery identity mismatch")
	}
	if s.TemplateCatalogSHA256 != "" {
		tc, currentSpec, err := d.validate(h.Config)
		if err != nil {
			unlock(lock)
			return err
		}
		if currentSpec != s.SpecHash {
			unlock(lock)
			return fmt.Errorf("recovery task specification changed")
		}
		if err = verifyResolvedState(tc, s); err != nil {
			unlock(lock)
			return err
		}
	}
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	v, e := d.owned(ctx, s)
	if e == nil && s.Exit == nil && v.Status != "Running" {
		e = fmt.Errorf("VM not running; do not recreate")
	}
	if e != nil {
		unlock(lock)
		return e
	}
	t, e := d.attach(h.Config, s, lock)
	if e != nil {
		unlock(lock)
		return e
	}
	d.tasks[h.Config.ID] = t
	d.emit(s, "recovered_same_instance")
	return nil
}
func (d *Driver) get(id string) (*task, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.tasks[id]
	if t == nil {
		return nil, fmt.Errorf("unknown task")
	}
	return t, nil
}
func result(s *State) *drivers.ExitResult {
	if s.Exit == nil {
		return nil
	}
	r := &drivers.ExitResult{ExitCode: s.Exit.Code, Signal: s.Exit.Signal}
	if s.Exit.Error != "" {
		r.Err = fmt.Errorf("guest execution failed: %s", s.Exit.Error)
	}
	return r
}
func (d *Driver) WaitTask(ctx context.Context, id string) (<-chan *drivers.ExitResult, error) {
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	ch := make(chan *drivers.ExitResult, 1)
	go func() {
		defer close(ch)
		select {
		case <-t.done:
			t.mu.Lock()
			ch <- result(t.state)
			t.mu.Unlock()
		case <-ctx.Done():
		case <-d.ctx.Done():
		}
	}()
	return ch, nil
}
func (d *Driver) InspectTask(id string) (*drivers.TaskStatus, error) {
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state
	phase := drivers.TaskStateRunning
	if s.Exit != nil {
		phase = drivers.TaskStateExited
	}
	if t.lastErr != nil {
		phase = drivers.TaskStateUnknown
	}
	return &drivers.TaskStatus{ID: id, Name: s.TaskName, State: phase, StartedAt: s.StartedAt, CompletedAt: s.CompletedAt, ExitResult: result(s), DriverAttributes: map[string]string{"instance": s.Instance, "guest_os": "linux", "guest_arch": runtime.GOARCH, "network_isolated": "false", "results_retained": "true", "stats_scope": "host VM process snapshot", "lifecycle_mode": "report-only", "owner_state": "owner_unconfirmed", "work_state": s.Lifecycle.WorkState, "auto_close": "false", "auto_delete": "false", "lifecycle_report": s.Lifecycle.Report}}, nil
}
func (d *Driver) SignalTask(id, sig string) error {
	t, e := d.get(id)
	if e != nil {
		return e
	}
	signals := map[string]int{"SIGTERM": 15, "SIGINT": 2, "SIGKILL": 9, "TERM": 15, "INT": 2, "KILL": 9}
	n, ok := signals[sig]
	if !ok {
		return fmt.Errorf("unsupported signal")
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
	defer cancel()
	if _, e = d.owned(ctx, &s); e != nil {
		return e
	}
	p := d.payload(&s, "signal")
	p["signal"] = n
	_, _, e = d.client.Guest(ctx, s.Instance, p)
	return e
}
func (d *Driver) StopTask(id string, timeout time.Duration, sig string) error {
	t, e := d.get(id)
	if e != nil {
		return e
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
	defer cancel()
	v, e := d.owned(ctx, &s)
	if e != nil {
		return e
	}
	if v.Status == "Running" {
		// Journal before signalling: recovery must never restart secret delivery
		// after cancellation/finish has begun, even if runtime shutdown is slow.
		t.vaultMu.Lock()
		t.vaultStopped = true
		t.mu.Lock()
		t.state.VaultFinalizing = true
		sealErr := d.store.write(t.state)
		t.mu.Unlock()
		t.vaultMu.Unlock()
		if sealErr != nil {
			return sealErr
		}
		if s.Exit == nil {
			if sig == "" {
				sig = "SIGTERM"
			}
			_ = d.SignalTask(id, sig)
			if timeout <= 0 {
				timeout = time.Second
			}
			select {
			case <-t.done:
			case <-time.After(timeout):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if _, e = d.owned(ctx, &s); e != nil {
			return e
		}
		t.vaultMu.Lock()
		er := d.finalizeVault(ctx, t.cfg, &s)
		t.vaultMu.Unlock()
		if er != nil {
			return er
		}
		var vaultTask TaskConfig
		_ = t.cfg.DecodeDriverConfig(&vaultTask)
		if vaultTask.VaultRequired {
			d.emit(&s, "task_token_revoked")
		}
		if e = d.client.Stop(ctx, s.Instance); e != nil {
			return e
		}
	}
	confirmed, err := d.owned(ctx, &s)
	if err != nil {
		return err
	}
	if confirmed.Status != "Stopped" || confirmed.DriverPID > 0 || confirmed.HostAgentPID > 0 {
		return fmt.Errorf("runtime stop not confirmed; VM/reservations retained")
	}
	t.mu.Lock()
	if t.state.Exit == nil {
		t.state.Exit = &lima.Result{Code: -1, Error: "stopped_before_exit_record"}
		t.state.CompletedAt = time.Now()

	}
	t.state.Phase = "stopped"
	e = d.store.write(t.state)
	t.mu.Unlock()
	t.once.Do(func() { close(t.done) })
	if e == nil {
		d.emit(&s, "vm_stopped")
	}
	return e
}
func (d *Driver) DestroyTask(id string, force bool) error {
	t, e := d.get(id)
	if e != nil {
		return e
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, 2*time.Minute)
	defer cancel()
	v, e := d.owned(ctx, &s)
	if e != nil {
		return e
	}
	if v.Status == "Running" {
		if s.Exit == nil && !force {
			return fmt.Errorf("active workload; stop explicitly before destroy")
		}
		if e = d.StopTask(id, time.Second, "SIGTERM"); e != nil {
			return e
		}
	}
	confirmed, err := d.owned(ctx, &s)
	if err != nil {
		return err
	}
	if confirmed.Status != "Stopped" || confirmed.DriverPID > 0 || confirmed.HostAgentPID > 0 {
		return fmt.Errorf("destroy requires confirmed stopped owned VM; retained")
	}
	t.mu.Lock()
	t.state.Destroyed = true
	t.state.Phase = "destroyed_disk_retained"
	e = d.store.write(t.state)
	t.mu.Unlock()
	return e
}
func (d *Driver) ExecTask(id string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	defer cancel()
	if _, e = d.owned(ctx, &s); e != nil {
		return nil, e
	}
	out, e := d.client.Exec(ctx, s.Instance, cmd)
	r := &drivers.ExecTaskResult{Stdout: out, ExitResult: &drivers.ExitResult{ExitCode: 0}}
	if e != nil {
		r.ExitResult.ExitCode = -1
		r.ExitResult.Err = e
		return r, e
	}
	return r, nil
}
func (d *Driver) Shutdown(ctx context.Context) error {
	d.cancel()
	d.mu.Lock()
	for _, t := range d.tasks {
		t.out.Close()
		t.err.Close()
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Non-TTY native Nomad exec streams; no wrapper server or alternate RPC.
func (d *Driver) ExecTaskStreaming(ctx context.Context, id string, o *drivers.ExecOptions) (*drivers.ExitResult, error) {
	if o == nil {
		return nil, fmt.Errorf("exec options required")
	}
	t, e := d.get(id)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	s := *t.state
	t.mu.Unlock()
	if _, e = d.owned(ctx, &s); e != nil {
		return nil, e
	}
	if o.Tty {
		sizes := make(chan lima.TerminalSize, 1)
		resizeCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			defer close(sizes)
			for {
				select {
				case <-resizeCtx.Done():
					return
				case v, ok := <-o.ResizeCh:
					if !ok {
						return
					}
					select {
					case sizes <- lima.TerminalSize{Rows: uint16(v.Height), Cols: uint16(v.Width)}:
					case <-resizeCtx.Done():
						return
					}
				}
			}
		}()
		e = d.client.PTY(ctx, s.Instance, o.Command, o.Stdin, o.Stdout, sizes)
	} else {
		e = d.client.Stream(ctx, s.Instance, o.Command, o.Stdin, o.Stdout, o.Stderr)
	}
	if e == nil {
		return &drivers.ExitResult{ExitCode: 0}, nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) && exit.ExitCode() != 255 && ctx.Err() == nil {
		return &drivers.ExitResult{ExitCode: exit.ExitCode()}, nil
	}
	return &drivers.ExitResult{ExitCode: -1, Err: fmt.Errorf("exec transport uncertain; no retry")}, e
}
