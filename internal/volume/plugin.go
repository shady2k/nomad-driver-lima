// Package volume implements the box external dynamic host volume plugin.
//
// Nomad invokes the plugin executable as `<executable> <operation>` with an
// environment that contains nothing but the DHV_* variables below, so the
// plugin must supply its own PATH/HOME for any child process it starts, and it
// reads where limactl lives, which Lima home to use and which user owns the
// volumes from a file the node role writes (see the Settings type). A volume
// request that carries those three is refused.
//
// The plugin owns one host directory per volume: <DHV_VOLUMES_DIR>/<DHV_VOLUME_ID>.
// The box task driver places a Lima disk file named box-<box_id>.qcow2 in it and
// symlinks that file into LIMA_HOME/<box_id>/disk, so the volume directory is
// what pins a box to its node. Deleting the volume therefore has to take the
// Lima instances that own those disks with it, otherwise lima keeps a stale
// instance (and an unusable disk) behind.
package volume

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/shady2k/nomad-driver-lima/internal/identity"
)

// PluginVersion is reported to Nomad by the `fingerprint` operation.
const PluginVersion = "0.1.0"

// Environment variables Nomad sets for each operation (see
// nomad/client/hostvolumemanager/host_volume_plugin.go).
const (
	envOperation   = "DHV_OPERATION"
	envVolumesDir  = "DHV_VOLUMES_DIR"
	envCreatedPath = "DHV_CREATED_PATH"
	envVolumeID    = "DHV_VOLUME_ID"
	envCapacityMin = "DHV_CAPACITY_MIN_BYTES"
	envCapacityMax = "DHV_CAPACITY_MAX_BYTES"
	envParameters  = "DHV_PARAMETERS"
)

// Operations accepted as the single command line argument.
const (
	opFingerprint = "fingerprint"
	opCreate      = "create"
	opDelete      = "delete"
)

const (
	// defaultLimactl is used when the settings file names no limactl binary.
	defaultLimactl = "limactl"
	// defaultPath is handed to limactl because Nomad gives the plugin no PATH.
	defaultPath = "/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin:/opt/homebrew/bin"
	// diskPrefix and diskSuffix frame the box id in a disk file name; this is
	// the exact naming the box task driver uses.
	diskPrefix = "box-"
	diskSuffix = ".qcow2"
	// volumeDirMode is the mode of a volume directory.
	volumeDirMode fs.FileMode = 0o755
)

// LimactlFunc runs limactl. args[0] is the limactl binary, the rest are its
// arguments; env is the complete environment for the child process and id the
// unprivileged user it runs as (nil means the plugin's own identity). Tests
// inject a recording fake so no real lima is needed.
type LimactlFunc func(args []string, env []string, id *identity.Identity) error

// Plugin is the volume plugin. The zero value is not usable; use New.
type Plugin struct {
	// Getenv reads the process environment. Tests inject a map lookup.
	Getenv func(string) string
	// Stdout receives the JSON response Nomad parses.
	Stdout io.Writer
	// Limactl runs limactl. Tests inject a fake.
	Limactl LimactlFunc
	// Executable returns the path of the plugin binary; the settings file of a
	// plugin that carries its own is <path>.json. Tests inject it.
	Executable func() (string, error)
	// Geteuid reports the effective uid; a plugin that runs as root needs the
	// settings file to name the runtime user. Tests inject it.
	Geteuid func() int
}

// New returns a Plugin wired to the real environment, stdout and limactl.
func New() *Plugin {
	return &Plugin{Getenv: os.Getenv, Stdout: os.Stdout, Limactl: execLimactl}
}

// Run is the entry point for cmd/box-volume. argv holds the arguments after
// the program name, so argv[0] is the operation Nomad requested. It returns the
// process exit code.
func Run(argv []string, getenv func(string) string, stdout io.Writer) int {
	return (&Plugin{Getenv: getenv, Stdout: stdout, Limactl: execLimactl}).Run(argv)
}

// Run dispatches one operation and returns the process exit code.
func (p *Plugin) Run(argv []string) int {
	if p.Getenv == nil {
		p.Getenv = os.Getenv
	}
	if p.Stdout == nil {
		p.Stdout = io.Discard
	}
	if p.Limactl == nil {
		p.Limactl = execLimactl
	}

	if len(argv) == 0 {
		return p.fail("missing operation: expected one of fingerprint, create, delete")
	}

	switch op := argv[0]; op {
	case opFingerprint:
		return p.writeJSON(fingerprintResponse{Version: PluginVersion})
	case opCreate:
		resp, err := p.create()
		if err != nil {
			return p.fail(err.Error())
		}
		return p.writeJSON(resp)
	case opDelete:
		resp, err := p.delete()
		if err != nil {
			return p.fail(err.Error())
		}
		return p.writeJSON(resp)
	default:
		return p.fail(fmt.Sprintf("unknown operation %q: expected one of fingerprint, create, delete", op))
	}
}

// fingerprintResponse is what Nomad reads as PluginFingerprint.
type fingerprintResponse struct {
	Version string `json:"version"`
}

// createResponse is what Nomad reads as HostVolumePluginCreateResponse.
type createResponse struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// deleteResponse is what Nomad reads as HostVolumePluginDeleteResponse.
type deleteResponse struct {
	Error string `json:"error"`
}

// errorResponse is printed when an operation fails.
type errorResponse struct {
	Error string `json:"error"`
}

// fail prints the error as JSON and returns a non-zero exit code.
func (p *Plugin) fail(msg string) int {
	p.writeJSON(errorResponse{Error: msg})
	return 1
}

func (p *Plugin) writeJSON(v any) int {
	if err := json.NewEncoder(p.Stdout).Encode(v); err != nil {
		return 1
	}
	return 0
}

// create makes the volume directory. It is idempotent: an existing directory is
// reused, which is what Nomad depends on because it calls create again for every
// volume when the client starts.
//
// The directory belongs to the runtime user: the box driver symlinks the box
// disk into it and runs limactl as that user, so a root-owned volume directory
// would refuse the disk.
func (p *Plugin) create() (createResponse, error) {
	dir, err := p.volumePath()
	if err != nil {
		return createResponse{}, err
	}
	settings, err := p.settings()
	if err != nil {
		return createResponse{}, err
	}
	id, err := p.identityFor(settings)
	if err != nil {
		return createResponse{}, err
	}

	created := false
	if _, statErr := os.Stat(dir); statErr != nil {
		if !errors.Is(statErr, fs.ErrNotExist) {
			return createResponse{}, fmt.Errorf("stat volume directory %q: %w", dir, statErr)
		}
		if mkErr := os.MkdirAll(dir, volumeDirMode); mkErr != nil {
			return createResponse{}, fmt.Errorf("create volume directory %q: %w", dir, mkErr)
		}
		created = true
	}

	if created {
		// MkdirAll applies the umask, so set the mode explicitly.
		if chmodErr := os.Chmod(dir, volumeDirMode); chmodErr != nil {
			return createResponse{}, fmt.Errorf("chmod volume directory %q: %w", dir, chmodErr)
		}
	}

	// The whole tree, not just the directory: a disk an earlier run created as
	// root would refuse qemu, which runs as the runtime user.
	if err := id.ChownTree(dir); err != nil {
		return createResponse{}, fmt.Errorf("give volume directory %q to %s: %w", dir, id.Name, err)
	}

	bytes, err := p.capacityBytes()
	if err != nil {
		return createResponse{}, err
	}

	return createResponse{Path: dir, Bytes: bytes}, nil
}

// capacityBytes reports the capacity the plugin claims: the requested maximum
// when set, else the requested minimum, else 0 for an unrestricted volume.
func (p *Plugin) capacityBytes() (int64, error) {
	maxBytes, err := p.envInt(envCapacityMax)
	if err != nil {
		return 0, err
	}
	if maxBytes > 0 {
		return maxBytes, nil
	}
	minBytes, err := p.envInt(envCapacityMin)
	if err != nil {
		return 0, err
	}
	if minBytes > 0 {
		return minBytes, nil
	}
	return 0, nil
}

func (p *Plugin) envInt(name string) (int64, error) {
	raw := strings.TrimSpace(p.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	return n, nil
}

// delete removes the volume directory together with the Lima instances whose
// disks live in it. It is idempotent: a directory that is already gone is a
// success.
func (p *Plugin) delete() (deleteResponse, error) {
	volumesDir, err := p.volumesDir()
	if err != nil {
		return deleteResponse{}, err
	}

	target, err := p.createdPath(volumesDir)
	if err != nil {
		return deleteResponse{}, err
	}

	settings, err := p.settings()
	if err != nil {
		return deleteResponse{}, err
	}

	id, err := p.identityFor(settings)
	if err != nil {
		return deleteResponse{}, err
	}

	disks, err := boxDisks(target)
	if err != nil {
		return deleteResponse{}, err
	}

	for _, boxID := range disks {
		p.dropInstance(settings, id, boxID)
	}

	if err := os.RemoveAll(target); err != nil {
		return deleteResponse{}, fmt.Errorf("remove volume directory %q: %w", target, err)
	}
	return deleteResponse{}, nil
}

// dropInstance stops and deletes one Lima instance that owns a disk in the
// volume. Both steps are best effort: a box that is already stopped, or an
// instance lima no longer knows about, must not block removing the volume.
func (p *Plugin) dropInstance(settings Settings, id *identity.Identity, boxID string) {
	env := settings.limactlEnv(id)
	// Ignore failures; the instance may be stopped, broken or absent.
	_ = p.Limactl([]string{settings.Limactl, "stop", "-f", boxID}, env, id)
	_ = p.Limactl([]string{settings.Limactl, "delete", boxID}, env, id)
}

// boxDisks returns the sorted box ids of the disk files in dir. A missing dir
// yields no ids; any other error is returned.
func boxDisks(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read volume directory %q: %w", dir, err)
	}

	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, diskPrefix) || !strings.HasSuffix(name, diskSuffix) {
			continue
		}
		boxID := strings.TrimSuffix(strings.TrimPrefix(name, diskPrefix), diskSuffix)
		if !validBoxID(boxID) {
			continue
		}
		seen[boxID] = struct{}{}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// validVolumeID reports whether s is a usable volume directory name: one path
// element, no "." or "..", no leading dash. Nomad generates the ID, but a bad
// environment value must never reach the filesystem.
func validVolumeID(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 || strings.HasPrefix(s, "-") {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

// validBoxID reports whether s can be used as a Lima instance name: the box
// driver allows [a-z0-9-]{1,40}, and this guard keeps a hostile file name from
// reaching limactl as a flag or a path.
func validBoxID(s string) bool {
	if s == "" || len(s) > 40 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// volumePath returns the absolute volume directory
// <DHV_VOLUMES_DIR>/<DHV_VOLUME_ID>.
func (p *Plugin) volumePath() (string, error) {
	dir, err := p.volumesDir()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.Getenv(envVolumeID))
	if !validVolumeID(id) {
		return "", fmt.Errorf("invalid or missing %s %q", envVolumeID, id)
	}
	return filepath.Join(dir, id), nil
}

// volumesDir returns the absolute volumes root.
func (p *Plugin) volumesDir() (string, error) {
	raw := strings.TrimSpace(p.Getenv(envVolumesDir))
	if raw == "" {
		return "", fmt.Errorf("missing %s", envVolumesDir)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", envVolumesDir, raw, err)
	}
	return filepath.Clean(abs), nil
}

// createdPath returns the directory delete should remove: the path create
// reported, falling back to <DHV_VOLUMES_DIR>/<DHV_VOLUME_ID>. The result must
// be a strict child of the volumes root, so a bad environment value can never
// make the plugin remove an unrelated tree.
func (p *Plugin) createdPath(volumesDir string) (string, error) {
	raw := strings.TrimSpace(p.Getenv(envCreatedPath))
	var target string
	if raw == "" {
		id := strings.TrimSpace(p.Getenv(envVolumeID))
		if !validVolumeID(id) {
			return "", fmt.Errorf("missing %s and invalid or missing %s %q",
				envCreatedPath, envVolumeID, id)
		}
		target = filepath.Join(volumesDir, id)
	} else {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", fmt.Errorf("resolve %s %q: %w", envCreatedPath, raw, err)
		}
		target = filepath.Clean(abs)
	}

	rel, err := filepath.Rel(volumesDir, target)
	if err != nil {
		return "", fmt.Errorf("volume path %q is not below %q: %w", target, volumesDir, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume path %q is not below volumes dir %q", target, volumesDir)
	}
	return target, nil
}

// Settings are the node operator's box-volume settings.
//
// They describe the node, not the volume: the limactl binary the plugin runs,
// the Lima home that holds the box instances and the unprivileged user that owns
// the volume directories. A volume request may not carry them (see
// Plugin.rejectParameters), so the node role writes them into a file.
type Settings struct {
	// Path is the file the settings were read from.
	Path string
	// Limactl is the limactl binary every limactl call runs.
	Limactl string
	// LimaHome is the LIMA_HOME directory holding the Lima instances.
	LimaHome string
	// RuntimeUser is the unprivileged user that owns the volume directories and
	// runs limactl. Empty means the plugin's own user.
	RuntimeUser string
}

// settingsFile is the JSON document of one settings file. Every key is optional;
// an unknown key is an error, so a typo is not silently ignored.
type settingsFile struct {
	Limactl     *string `json:"limactl"`
	LimaHome    *string `json:"lima_home"`
	RuntimeUser *string `json:"runtime_user"`
}

// Settings file locations, tried in order.
const (
	// defaultSettingsPath is the file the node role writes.
	defaultSettingsPath = "/etc/box/volume.json"
	// settingsEnv names a settings file explicitly. Nomad gives a host volume
	// plugin no environment beyond DHV_*, so a node does not use this: it is for
	// running the plugin by hand and for tests.
	settingsEnv = "BOX_VOLUME_CONFIG"
	// settingsSuffix is appended to the path of the plugin executable: a plugin
	// that carries its own settings (a release install, a test harness).
	settingsSuffix = ".json"
)

// settings reads the node operator's settings for this plugin.
func (p *Plugin) settings() (Settings, error) {
	if err := p.rejectParameters(); err != nil {
		return Settings{}, err
	}
	path, err := p.settingsPath()
	if err != nil {
		return Settings{}, err
	}
	s, err := readSettings(path)
	if err != nil {
		return Settings{}, err
	}
	if s.LimaHome != "" {
		abs, err := filepath.Abs(s.LimaHome)
		if err != nil {
			return Settings{}, fmt.Errorf("resolve lima_home %q in %s: %w", s.LimaHome, path, err)
		}
		s.LimaHome = abs
		return s, nil
	}
	// No lima_home: the runtime user's own ~/.lima, or the plugin's own.
	home := p.homeDir()
	if s.RuntimeUser != "" {
		id, err := identity.Resolve(s.RuntimeUser)
		if err != nil {
			return Settings{}, fmt.Errorf("%s: %w", path, err)
		}
		home = id.Home
	}
	if home == "" {
		return Settings{}, fmt.Errorf("no lima_home in %s and no home directory to default to: set lima_home", path)
	}
	s.LimaHome = filepath.Join(home, ".lima")
	return s, nil
}

// rejectParameters refuses a volume request that carries settings of the node.
//
// A dynamic host volume is declared by whoever creates the volume (a
// coordinator, a job spec, another cluster's operator) and none of them may
// decide which binary runs on the node, which Lima home it uses or which user
// owns the volumes: those three are the node's own configuration, and taking
// them from the request is what puts the coordinator in charge of the node.
func (p *Plugin) rejectParameters() error {
	raw := strings.TrimSpace(p.Getenv(envParameters))
	if raw == "" || raw == "{}" || raw == "null" {
		return nil
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return fmt.Errorf("parse %s as a JSON object: %w", envParameters, err)
	}
	if len(decoded) == 0 {
		return nil
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fmt.Errorf("the volume request carries parameters (%s): the box-volume plugin takes none — limactl, lima_home and runtime_user are settings of the node, not of the volume, and the node role writes them into %s",
		strings.Join(keys, ", "), defaultSettingsPath)
}

// settingsPath returns the first settings file that exists.
func (p *Plugin) settingsPath() (string, error) {
	if explicit := strings.TrimSpace(p.Getenv(settingsEnv)); explicit != "" {
		st, err := os.Stat(explicit)
		if err != nil || !st.Mode().IsRegular() {
			return "", fmt.Errorf("%s names %s, which is not a readable file: %v", settingsEnv, explicit, err)
		}
		return explicit, nil
	}
	candidates := []string{defaultSettingsPath}
	if exe, err := p.executable(); err == nil && exe != "" {
		candidates = append(candidates, exe+settingsSuffix)
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			return path, nil
		}
	}
	return "", fmt.Errorf("no box-volume settings file: the node role writes %s with limactl, lima_home and runtime_user; looked for %s",
		defaultSettingsPath, strings.Join(candidates, ", "))
}

// executable returns the path of the plugin binary, which is where a plugin
// that carries its own settings file finds it.
func (p *Plugin) executable() (string, error) {
	if p.Executable != nil {
		return p.Executable()
	}
	return os.Executable()
}

// readSettings decodes one settings file.
func readSettings(path string) (Settings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("read %s: %w", path, err)
	}
	var file settingsFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return Settings{}, fmt.Errorf(`%s is not a box-volume settings file ({"limactl": "/usr/local/bin/limactl", "lima_home": "/var/lib/box/lima", "runtime_user": "box"}): %w`, path, err)
	}
	s := Settings{Path: path, Limactl: defaultLimactl}
	if file.Limactl != nil {
		if v := strings.TrimSpace(*file.Limactl); v != "" {
			s.Limactl = v
		}
	}
	if file.LimaHome != nil {
		s.LimaHome = strings.TrimSpace(*file.LimaHome)
	}
	if file.RuntimeUser != nil {
		s.RuntimeUser = strings.TrimSpace(*file.RuntimeUser)
	}
	return s, nil
}

// identityFor is the user the plugin gives its volume directories to and runs
// limactl as.
//
// The settings file names it. A plugin that runs as an unprivileged user and has
// no runtime_user uses its own identity: the box driver then gives the volume
// directory to the runtime user at the first box start, so such a volume works.
// A plugin that runs as root must be told, because Lima refuses to run as root
// and the failure would only show up later, at delete time.
func (p *Plugin) identityFor(s Settings) (*identity.Identity, error) {
	if s.RuntimeUser == "" {
		if p.euid() == 0 {
			return nil, fmt.Errorf("%s names no runtime_user and the plugin runs as root: Lima refuses to run as root, so the node role must set runtime_user to the unprivileged user the box driver runs Lima as", s.Path)
		}
		return identity.Current(), nil
	}
	id, err := identity.Resolve(s.RuntimeUser)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return id, nil
}

// euid is the effective uid of the plugin process.
func (p *Plugin) euid() int {
	if p.Geteuid != nil {
		return p.Geteuid()
	}
	return os.Geteuid()
}

// homeDir returns the home directory the plugin should use for defaults. Nomad
// hands the plugin an environment without HOME, so fall back to the passwd
// entry; os.UserHomeDir reads only HOME and would return "".
func (p *Plugin) homeDir() string {
	if home := strings.TrimSpace(p.Getenv("HOME")); home != "" {
		return home
	}
	if u, err := user.Current(); err == nil {
		return strings.TrimSpace(u.HomeDir)
	}
	return ""
}

// limactlEnv is the complete environment given to limactl: Nomad's DHV_* only
// environment would leave lima without a PATH or a home.
//
// It is the runtime user's environment, not the plugin's: limactl runs as that
// user, so it needs that user's HOME and runtime directory, and the same
// explicit set the box driver uses (see internal/identity).
func (s Settings) limactlEnv(id *identity.Identity) []string {
	if id != nil {
		return id.Env(s.LimaHome)
	}
	return []string{"PATH=" + defaultPath, "LIMA_HOME=" + s.LimaHome}
}

// execLimactl runs the real limactl binary as the runtime user.
func execLimactl(args []string, env []string, id *identity.Identity) error {
	if len(args) == 0 {
		return errors.New("empty limactl command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	if id != nil {
		id.Configure(cmd)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
