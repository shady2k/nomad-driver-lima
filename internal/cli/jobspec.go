package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// The box conventions in Nomad. The driver already supports these shapes
// (see docs/boxctl-contract.md), so the command only has to follow them.
const (
	// boxJobPrefix starts the job name of a box: box-<id>.
	boxJobPrefix = "box-"
	// boxGroupName and boxTaskName are the group and task of a box job.
	boxGroupName = "box"
	boxTaskName  = "box"
	// boxDriverName is the Nomad task driver plugin, installed as "box".
	boxDriverName = "box"
	// boxVolumePrefix starts the dynamic host volume name: box-<id>.
	boxVolumePrefix = "box-"
	// boxBakeJobPrefix and boxPublishJobPrefix start the two batch jobs the CLI
	// registers for an image: box-bake-<box-id> and box-publish-<version>. They
	// are not boxes, and their names are not box ids.
	boxBakeJobPrefix    = "box-bake-"
	boxPublishJobPrefix = "box-publish-"
	// boxWorkMarker is the meta key that marks a job as a step of an image's
	// life (a bake or a publish) instead of a box: `box ls` and the capacity
	// count skip it.
	boxWorkMarker = "box_work"
	// boxVolumePluginID is the dynamic host volume plugin that creates the disk.
	boxVolumePluginID = "box-volume"
	// boxDiskVolume is the group volume and volume_mount name of the disk.
	boxDiskVolume = "disk"
	// boxDiskMount is where the disk volume is mounted in the task environment.
	boxDiskMount = "/disk"
	// boxFilesDir is where the driver copies the task files in the guest.
	boxFilesDir = "/etc/box"
	// boxMemoryOverheadMiB is the memory the VM needs on top of the guest RAM.
	// It matches the driver default (memory_overhead_mib = 512).
	BoxMemoryOverheadMiB = 512
	// boxMetaMarker is the meta key that marks a job as a box.
	boxMetaMarker = "box"
	// boxStopTimeout is how long the driver may take to stop a VM on its own.
	boxStopTimeout = 90 * time.Second
	// boxHealthyDeadline is how long a new allocation may take to report a
	// running task. A box boots a VM, so it needs more than Nomad's 5 minutes;
	// Nomad rejects a healthy deadline that is not below the progress deadline,
	// which defaults to 10 minutes.
	boxHealthyDeadline  = 15 * time.Minute
	boxProgressDeadline = 20 * time.Minute
)

// boxdLogCommand streams the box daemon journal into the Nomad task log; the
// guest boxctl events are the resumable form of the same stream.
//
// The task runs as the box user, and a journal is only readable for a member of
// adm or systemd-journal, so the command asks sudo: otherwise `box logs` would be
// empty while boxd has plenty to say. sudo is passwordless in a box guest.
var boxdLogCommand = []string{"sudo", "-n", "journalctl", "-f", "-o", "cat", "-u", "boxd"}

// boxIDPattern is the box id the driver accepts: [a-z0-9-], at most 40
// characters, and it must not begin or end with a dash.
var boxIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// BoxSpec is everything that decides the shape of one box.
type BoxSpec struct {
	// CPUMHzPerVCPU is what one vCPU reserves on the box's node (0: the default).
	CPUMHzPerVCPU int
	// ID is the box id; the job is box-<id> and the volume box-<id>.
	ID string
	// ImageVersion is the image version the box was created from.
	ImageVersion string
	// ImageURL and ImageSHA256 locate the base image.
	ImageURL    string
	ImageSHA256 string
	// PlatformVersion, PlatformURL and PlatformSHA256 name the platform bundle
	// the node mounts into the guest at /opt/box. All three empty means no
	// bundle: the image runs the boxd it carries.
	PlatformVersion string
	PlatformURL     string
	PlatformSHA256  string
	// Arch is amd64 or arm64.
	Arch string
	// CPUs is the number of guest vCPUs.
	CPUs int
	// MemoryMiB is the guest RAM, without the driver overhead.
	MemoryMiB int
	// DiskGiB is the size of the box disk.
	DiskGiB int
	// NodeID pins the box to one node. It is only used when the volume is
	// created; the job itself is pinned by the volume.
	NodeID string
	// Project is the project the box belongs to, a label in the job's meta and so
	// in its stored description. Empty means the box carries no label; the box is
	// then shown with a "-" and nothing else changes.
	Project string
	// JobName is what Nomad shows as the job's name: "<project>/box-<id>" for a box
	// created with a project, so the jobs page says whose it is, and the job id
	// otherwise. It is decided once, when the box is created: a box keeps the name
	// it has through a resize, an upgrade or a start.
	JobName string
	// MemoryOverheadMiB is added to MemoryMiB for the task resources. Zero
	// means BoxMemoryOverheadMiB.
	MemoryOverheadMiB int
	// GuestLogCommand is the command whose output becomes the task log.
	GuestLogCommand []string
	// FilesDirInGuest is where the driver copies the task files.
	FilesDirInGuest string
	// ExecutorItems are the items of the executors Variable a box renders.
	// Each one becomes a template that writes a file into the task's secrets
	// directory; empty means the box starts without executor credentials. An item
	// whose key ends in .tmpl carries its own template text, which reads the
	// secret from Vault, and is the reason the job has a Vault block.
	ExecutorItems []ExecutorItem
	// VaultRole is the Vault role the task reads its secrets with. Empty means
	// the cluster's default role and the job carries no role at all.
	VaultRole string
	// Recipe is the script that makes this image's environment from its parent
	// image, base64. A box created from a project image carries it, so a box of
	// that image that is baked again, or rebuilt on a newer parent, has the
	// recipe it came from. Empty means the image has none.
	Recipe string
}

// BoxJobName returns the Nomad job name of a box.
func BoxJobName(id string) string { return boxJobPrefix + id }

// BoxVolumeName returns the dynamic host volume name of a box.
func BoxVolumeName(id string) string { return boxVolumePrefix + id }

// projectJobName is the name a new box's job is shown under: its project, then
// its job id. The id stays what every command and link uses.
func projectJobName(project, id string) string {
	if project == "" {
		return ""
	}
	return project + "/" + BoxJobName(id)
}

// jobDisplayName is the name a box's job is registered under: the one the box
// has, or the job id for a box that has none.
func jobDisplayName(spec BoxSpec, jobID string) string {
	if spec.JobName != "" {
		return spec.JobName
	}
	return jobID
}

// BoxBakeJobName returns the Nomad job name of a box's bake.
func BoxBakeJobName(id string) string { return boxBakeJobPrefix + id }

// BoxPublishJobName returns the Nomad job name of a version's publish.
func BoxPublishJobName(version string) string { return boxPublishJobPrefix + version }

// BoxIDFromJobName strips the job prefix from a job name.
//
// The batch jobs that make an image are not boxes: box-bake-<id> and
// box-publish-<version> take a box id and a version as their suffix, and a
// reader that took them for boxes would list an image as a box and count it
// against the node.
func BoxIDFromJobName(name string) (string, bool) {
	if strings.HasPrefix(name, boxBakeJobPrefix) || strings.HasPrefix(name, boxPublishJobPrefix) {
		return "", false
	}
	if !strings.HasPrefix(name, boxJobPrefix) {
		return "", false
	}
	return strings.TrimPrefix(name, boxJobPrefix), true
}

// ValidateBoxID reports whether id is a usable box id.
func ValidateBoxID(id string) error {
	if id == "" {
		return usagef("a box id is required, for example: box create dev1 --image 1.0.0 ...")
	}
	if !boxIDPattern.MatchString(id) || len(id) > 40 {
		return usagef("box id %q is not usable: use 1 to 40 characters from a-z, 0-9 and -,"+
			" starting and ending with a letter or a digit", id)
	}
	return nil
}

// overhead returns the memory the VM needs on top of the guest RAM.
func (s BoxSpec) overhead() int {
	if s.MemoryOverheadMiB > 0 {
		return s.MemoryOverheadMiB
	}
	return BoxMemoryOverheadMiB
}

// TaskMemoryMiB is the memory Nomad reserves for the task: guest RAM plus the
// overhead of the virtual machine itself.
func (s BoxSpec) TaskMemoryMiB() int { return s.MemoryMiB + s.overhead() }

// TaskCPUShares is the Nomad CPU reservation: what one vCPU reserves on the box's
// node, for each vCPU.
func (s BoxSpec) TaskCPUShares() int {
	if s.CPUMHzPerVCPU > 0 {
		return s.CPUs * s.CPUMHzPerVCPU
	}
	return s.CPUs * boxCPUSharesPerVCPU
}

// DiskBytes is the size of the disk in bytes.
func (s BoxSpec) DiskBytes() int64 { return int64(s.DiskGiB) * (1 << 30) }

// Validate checks that the spec can be rendered into a job and a volume.
func (s BoxSpec) Validate() error {
	if err := ValidateBoxID(s.ID); err != nil {
		return err
	}
	if s.ImageURL == "" || s.ImageSHA256 == "" {
		return usagef("no image is set for the box: use --image <version> and let box resolve it from the image index")
	}
	switch NormalizeArch(s.Arch) {
	case "amd64", "arm64":
	case "":
		return usagef("no architecture is set for the box: use --arch amd64 or --arch arm64")
	default:
		return usagef("architecture %q is not supported: use amd64 or arm64", s.Arch)
	}
	if s.CPUs <= 0 {
		return usagef("--cpus must be at least 1, got %d", s.CPUs)
	}
	if s.MemoryMiB < 256 {
		return usagef("--memory-mib must be at least 256, got %d", s.MemoryMiB)
	}
	if s.DiskGiB < 1 {
		return usagef("--disk-gib must be at least 1, got %d", s.DiskGiB)
	}
	// A bundle is mounted by url and checked by sha256: half of the pair would
	// mount whatever the url happens to serve.
	if s.PlatformVersion != "" || s.PlatformURL != "" || s.PlatformSHA256 != "" {
		if s.PlatformURL == "" || s.PlatformSHA256 == "" {
			return usagef("the platform bundle of box %s needs both a url and a sha256, got url %q and sha256 %q",
				s.ID, s.PlatformURL, s.PlatformSHA256)
		}
	}
	return nil
}

// LimaArch maps an image architecture onto the lima/`driver.box.arch` spelling
// the job constraint uses.
func LimaArch(arch string) string {
	switch NormalizeArch(arch) {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return arch
	}
}

func strPtr(s string) *string               { return &s }
func intPtr(i int) *int                     { return &i }
func boolPtr(b bool) *bool                  { return &b }
func durPtr(d time.Duration) *time.Duration { return &d }

// RenderJob builds the Nomad job of a box.
//
// The shape follows examples/box.nomad.hcl: one service group with one box
// task, the disk on a dynamic host volume, the disconnect and restart policies
// the driver needs to survive a client restart, and an architecture
// constraint.
func RenderJob(spec BoxSpec) (*api.Job, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	spec.Arch = NormalizeArch(spec.Arch)
	jobID := BoxJobName(spec.ID)
	volumeName := BoxVolumeName(spec.ID)
	reconcile := api.ReconcileOptionKeepOriginal

	config := map[string]any{
		"box_id": spec.ID,
		"image": map[string]any{
			"url":    spec.ImageURL,
			"sha256": spec.ImageSHA256,
		},
		"cpus":               spec.CPUs,
		"memory_mib":         spec.MemoryMiB,
		"disk_gib":           spec.DiskGiB,
		"guest_log_command":  spec.guestLogCommand(),
		"files_dir_in_guest": spec.filesDir(),
	}
	// The recipe travels in the job's task config: it is what an image's record
	// carries, and a box baked from this one passes it on (see `box bake
	// --rebuild`).
	if spec.Recipe != "" {
		config["recipe"] = spec.Recipe
	}
	// A box made of an image that carries its own boxd has no platform block at
	// all, so its job is exactly the job it was before bundles existed.
	if spec.PlatformURL != "" && spec.PlatformSHA256 != "" {
		config["platform"] = map[string]any{
			"version": spec.PlatformVersion,
			"url":     spec.PlatformURL,
			"sha256":  spec.PlatformSHA256,
		}
	}

	task := &api.Task{
		Name:   boxTaskName,
		Driver: boxDriverName,
		Config: config,
		Resources: &api.Resources{
			CPU:      intPtr(spec.TaskCPUShares()),
			MemoryMB: intPtr(spec.TaskMemoryMiB()),
		},
		VolumeMounts: []*api.VolumeMount{{
			Volume:      strPtr(boxDiskVolume),
			Destination: strPtr(boxDiskMount),
		}},
		// Stopping a VM takes longer than Nomad's 5 second default, and longer
		// than Nomad's own cap: the client trims this value to its
		// `max_kill_timeout` (30s by default, 2m in deploy/ansible), and at 30s
		// the client cut `limactl stop` off mid-shutdown and the guest was
		// powered off hard. The driver's own stop timeout is 60 seconds.
		KillTimeout: durPtr(boxStopTimeout),
		// The executor credentials arrive as task files, one template per item,
		// so the job never carries a secret value: a plain item is read out of
		// the Nomad Variable, a .tmpl item reads Vault at render time.
		Templates: executorTemplates(spec.ExecutorItems),
		Vault:     spec.vaultBlock(),
	}

	group := &api.TaskGroup{
		Name:  strPtr(boxGroupName),
		Count: intPtr(1),
		// A client restart must not create a replacement allocation: the
		// driver re-attaches to the running VM.
		Disconnect: &api.DisconnectStrategy{
			LostAfter: durPtr(10 * time.Minute),
			Replace:   boolPtr(false),
			Reconcile: &reconcile,
		},
		// A box boots a whole Linux VM, and a busy node (or the guest's own first
		// boot) can take longer than Nomad's 5 minute default before the task
		// reports running; Nomad kills a task that misses that deadline
		// ("Task not running by healthy_deadline of 5m0s").
		Update: &api.UpdateStrategy{
			HealthyDeadline:  durPtr(boxHealthyDeadline),
			ProgressDeadline: durPtr(boxProgressDeadline),
		},
		// If the guest dies, the task restarts in the same allocation, which
		// is where the driver adopts the VM and its disk.
		RestartPolicy: &api.RestartPolicy{
			Attempts: intPtr(3),
			Delay:    durPtr(3 * time.Second),
			Interval: durPtr(10 * time.Minute),
			Mode:     strPtr("fail"),
		},
		Volumes: map[string]*api.VolumeRequest{
			boxDiskVolume: {
				Name:     boxDiskVolume,
				Type:     "host",
				Source:   volumeName,
				ReadOnly: false,
			},
		},
		Tasks: []*api.Task{task},
	}

	// The project is a label of the box, carried in the job's meta and so in its
	// stored description: a resize, an upgrade or a start reads it back and writes
	// it again, so the label survives every re-registration.
	meta := map[string]string{
		boxMetaMarker: "true",
		"box_id":      spec.ID,
		"image":       spec.ImageVersion,
		"arch":        spec.Arch,
	}
	if spec.Project != "" {
		meta["project"] = spec.Project
	}

	job := &api.Job{
		ID:          strPtr(jobID),
		Name:        strPtr(jobDisplayName(spec, jobID)),
		Type:        strPtr("service"),
		Datacenters: []string{"*"},
		// Datacenters are chosen by the volume: the box disk lives on one node.
		Constraints: []*api.Constraint{{
			LTarget: "${attr.driver.box.arch}",
			Operand: "=",
			RTarget: LimaArch(spec.Arch),
		}},
		Meta:       meta,
		TaskGroups: []*api.TaskGroup{group},
	}
	return job, nil
}

// vaultBlock is the task's Vault block, or nil when the box reads no secret.
//
// A .tmpl item renders `secret "kv/data/..."`, which Nomad can only resolve for
// a task with a Vault block: the block is what makes the client hand the task a
// token from the workload identity. Without a template item the job carries no
// block at all, so a box made of plain Variable items still runs on a cluster
// that has no Vault.
func (s BoxSpec) vaultBlock() *api.Vault {
	if !hasExecutorTemplate(s.ExecutorItems) {
		return nil
	}
	// noop: a renewed, expired or revoked token must not restart the VM. The
	// driver copies a re-rendered file into the guest and calls boxctl reload,
	// which is the same path a changed Variable takes.
	mode := "noop"
	return &api.Vault{
		Role:       strings.TrimSpace(s.VaultRole),
		ChangeMode: &mode,
		// The rendered files are the only secret a box gets. The token files
		// Nomad writes into secrets/ otherwise would land in the guest: the
		// driver copies that whole directory to its tmpfs.
		DisableFile: boolPtr(true),
		// The task process is the VM, not an executor, so no VAULT_* variable
		// has a reason to be in its environment.
		Env: boolPtr(false),
	}
}

func (s BoxSpec) guestLogCommand() []string {
	if len(s.GuestLogCommand) > 0 {
		return append([]string(nil), s.GuestLogCommand...)
	}
	return append([]string(nil), boxdLogCommand...)
}

func (s BoxSpec) filesDir() string {
	if s.FilesDirInGuest != "" {
		return s.FilesDirInGuest
	}
	return boxFilesDir
}

// VolumeSpec is the input of RenderVolume.
//
// It carries no parameters: which limactl binary a node runs, which Lima home it
// uses and which user owns the volumes are the node's own settings, and the
// box-volume plugin refuses a volume that carries them (it reads them from the
// node's settings file). The coordinator therefore declares the volume and
// nothing else.
type VolumeSpec struct {
	// BoxID names the volume: box-<id>.
	BoxID string
	// NodeID places the volume; an empty value lets the server choose.
	NodeID string
	// DiskGiB is the requested capacity.
	DiskGiB int
}

// RenderVolume builds the dynamic host volume record of a box disk.
func RenderVolume(spec VolumeSpec) (*api.HostVolume, error) {
	if err := ValidateBoxID(spec.BoxID); err != nil {
		return nil, err
	}
	if spec.DiskGiB < 1 {
		return nil, usagef("--disk-gib must be at least 1, got %d", spec.DiskGiB)
	}
	bytes := int64(spec.DiskGiB) * (1 << 30)
	return &api.HostVolume{
		Name:                      BoxVolumeName(spec.BoxID),
		PluginID:                  boxVolumePluginID,
		NodeID:                    spec.NodeID,
		RequestedCapacityMinBytes: bytes,
		RequestedCapacityMaxBytes: bytes,
		RequestedCapabilities: []*api.HostVolumeCapability{{
			AttachmentMode: api.HostVolumeAttachmentModeFilesystem,
			AccessMode:     api.HostVolumeAccessModeSingleNodeWriter,
		}},
	}, nil
}

// SpecFromJob reads a box spec back out of a registered job.
//
// This is how `box start`, `box resize` and `box status` see a box: Nomad holds
// the state, the command keeps none.
func SpecFromJob(job *api.Job) (BoxSpec, error) {
	if job == nil {
		return BoxSpec{}, errorf("no job was returned by Nomad")
	}
	spec := BoxSpec{JobName: derefStr(job.Name, "")}
	if job.Meta != nil {
		spec.ID = job.Meta["box_id"]
		spec.ImageVersion = job.Meta["image"]
		spec.Arch = NormalizeArch(job.Meta["arch"])
		spec.Project = job.Meta["project"]
	}
	if spec.ID == "" {
		if id, ok := BoxIDFromJobName(derefStr(job.ID, derefStr(job.Name, ""))); ok {
			spec.ID = id
		}
	}
	task, err := boxTask(job)
	if err != nil {
		return BoxSpec{}, err
	}
	cfg := task.Config
	spec.CPUs = intFromConfig(cfg, "cpus", 0)
	if task.Resources != nil && task.Resources.CPU != nil && spec.CPUs > 0 {
		spec.CPUMHzPerVCPU = *task.Resources.CPU / spec.CPUs
	}
	spec.MemoryMiB = intFromConfig(cfg, "memory_mib", 0)
	spec.DiskGiB = intFromConfig(cfg, "disk_gib", 0)
	if image, ok := configBlock(cfg, "image"); ok {
		if v, ok := image["url"].(string); ok {
			spec.ImageURL = v
		}
		if v, ok := image["sha256"].(string); ok {
			spec.ImageSHA256 = v
		}
	}
	if platform, ok := configBlock(cfg, "platform"); ok {
		if v, ok := platform["version"].(string); ok {
			spec.PlatformVersion = v
		}
		if v, ok := platform["url"].(string); ok {
			spec.PlatformURL = v
		}
		if v, ok := platform["sha256"].(string); ok {
			spec.PlatformSHA256 = v
		}
	}
	if v, ok := cfg["files_dir_in_guest"].(string); ok {
		spec.FilesDirInGuest = v
	}
	if v, ok := cfg["recipe"].(string); ok {
		spec.Recipe = v
	}
	// The role is read back so a start or a resize keeps the role the box was
	// created with: re-rendering the job with another role would hand the box a
	// different identity than its creator chose.
	if task.Vault != nil {
		spec.VaultRole = task.Vault.Role
	}
	if list, ok := cfg["guest_log_command"].([]any); ok {
		cmd := make([]string, 0, len(list))
		for _, v := range list {
			if s, ok := v.(string); ok {
				cmd = append(cmd, s)
			}
		}
		spec.GuestLogCommand = cmd
	}
	if spec.Arch == "" {
		if v, ok := job.Meta["arch"]; ok {
			spec.Arch = NormalizeArch(v)
		}
	}
	if task.Resources != nil && task.Resources.MemoryMB != nil {
		if spec.MemoryMiB == 0 {
			spec.MemoryMiB = *task.Resources.MemoryMB - BoxMemoryOverheadMiB
		}
		spec.MemoryOverheadMiB = *task.Resources.MemoryMB - spec.MemoryMiB
	}
	return spec, nil
}

// boxTask returns the box task of a job.
func boxTask(job *api.Job) (*api.Task, error) {
	for _, group := range job.TaskGroups {
		for _, task := range group.Tasks {
			if task.Name == boxTaskName {
				return task, nil
			}
		}
	}
	return nil, errorf("the job %q has no %q task, so it is not a box", derefStr(job.ID, ""), boxTaskName)
}

// IsBoxJob reports whether a job is a box: meta.box = "true" on a box-<id> job.
func IsBoxJob(job *api.Job) bool {
	if job == nil || job.Meta == nil {
		return false
	}
	return job.Meta[boxMetaMarker] == "true"
}

// IsBoxJobStub reports whether a job stub is a box.
func IsBoxJobStub(stub *api.JobListStub) bool {
	if stub == nil || stub.Meta == nil {
		return false
	}
	return stub.Meta[boxMetaMarker] == "true"
}

func derefStr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func derefInt(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}

// configBlock returns one block of a task's config as a map. The CLI renders a
// block as a map, and a job written in HCL reaches Nomad as a list with one map
// in it; both are the same block, and a box registered by hand is started and
// resized like one the CLI made.
func configBlock(cfg map[string]any, key string) (map[string]any, bool) {
	switch v := cfg[key].(type) {
	case map[string]any:
		return v, true
	case []any:
		if len(v) == 1 {
			if m, ok := v[0].(map[string]any); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func intFromConfig(cfg map[string]any, key string, fallback int) int {
	v, ok := cfg[key]
	if !ok {
		return fallback
	}
	return toInt(v, fallback)
}

// toInt converts the JSON numbers of a task config to int.
func toInt(v any, fallback int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return fallback
}

// TaskConfigFor returns the box task config of a job as a plain map, which is
// what `box status --json` prints.
func TaskConfigFor(job *api.Job) (map[string]any, error) {
	task, err := boxTask(job)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(task.Config))
	keys := make([]string, 0, len(task.Config))
	for k := range task.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = task.Config[k]
	}
	return out, nil
}

// describeSpec renders a spec as the one line status prints.
func describeSpec(spec BoxSpec) string {
	return fmt.Sprintf("%d vCPU, %d MiB RAM, %d GiB disk, image %s (%s)",
		spec.CPUs, spec.MemoryMiB, spec.DiskGiB, spec.ImageVersion, spec.Arch)
}
