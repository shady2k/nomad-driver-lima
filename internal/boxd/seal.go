package boxd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The seal turns a box into an image.
//
// An agent takes a box from a base or a project image, installs what its project
// needs, and the box is baked: the disk becomes an immutable image that carries
// the dependencies and the caches and none of the work. So a seal removes
// everything that belongs to one project or one credential -- the sources, the
// sessions and their transcripts, the credentials and every copy of them, the
// journal, the SSH host keys, the machine id -- and keeps what an image is for:
// the installed packages, the toolchains and the language environments in the
// executor homes.
//
// The seal refuses to finish on an unclean result: the bundle's own
// guest/no-secrets.sh is the proof it runs before it says it is done. Some of the
// box is gone by then, so a failed seal says so as well; the box a seal runs in
// is a clone of the box it came from, so nothing the owner needs is lost.

// imageRecordPath is where a sealed image records what it is. images/build.sh
// writes the same file for a built image, so a booted box answers "what am I"
// without the manifest at hand.
const imageRecordPath = "/etc/box/image.json"

// recipePath is where a sealed image keeps the recipe that makes its environment
// from its parent image; `box bake --rebuild` replays it on a newer parent.
const recipePath = "/etc/box/recipe.sh"

// DefaultWorkspaceRoot is where a box's sources and the work of its sessions
// live: the root a seal deletes.
const DefaultWorkspaceRoot = "/work"

// RecipeLimit is the largest recipe a seal accepts. The recipe travels in the
// image's record, in the job of the box baked from it and in the index entry, so
// it stays small.
const RecipeLimit = 64 << 10

// imageNamePattern and imageVersionPattern are what an image's name and version
// may look like: lower-case letters, digits and dashes for the name, and dots as
// well for the version ("<name>-<number>").
var (
	imageNamePattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	imageVersionPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
)

// ImageRecord is what a sealed box writes to /etc/box/image.json: the record of
// the image the box now is.
type ImageRecord struct {
	Schema  int    `json:"schema"`
	Version string `json:"version,omitempty"`
	Name    string `json:"name"`
	Arch    string `json:"arch,omitempty"`
	// Parent is the image the box came from: what a newer base image is measured
	// against when the recipe is replayed.
	Parent string `json:"parent,omitempty"`
	// Platform says a bundle completes this image ("mount"): boxd and the
	// executors are not in it, they come from the node's bundle at /opt/box.
	Platform string `json:"platform"`
	// Recipe is the script that makes this environment from the parent image,
	// base64 so that a script with any byte in it travels unchanged. It is also
	// at /etc/box/recipe.sh, and empty when the image has no recipe.
	Recipe   string `json:"recipe,omitempty"`
	SealedAt string `json:"sealed_at"`
}

// SealRequest is what `boxctl seal` asks the daemon to do.
type SealRequest struct {
	// Name is the image's name. The version is computed by the caller from the
	// images and the drafts it can see, and only recorded here.
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Parent  string `json:"parent,omitempty"`
	// Recipe is the recipe script, base64, up to RecipeLimit bytes decoded.
	Recipe string `json:"recipe,omitempty"`
}

// SealResult is what a seal leaves behind: the image record it wrote and what it
// did to get there.
type SealResult struct {
	Image ImageRecord `json:"image"`
	// Steps is what the seal deleted or proved, in the order it happened.
	Steps []string `json:"steps"`
}

// SealPaths is where a seal cleans and where it proves the result.
//
// Every path is a field so that the seal can be tested without emptying the
// machine it runs on: boxd fills them from the guest's own layout, and a test
// points them at a temporary tree.
type SealPaths struct {
	// Workspace is the box's workspace root, deleted whole.
	Workspace string
	// Homes are the executor homes to clean and every user's home.
	Homes []string
	// Secrets are the directories whose file names no longer belong in an
	// image: the driver's credentials and boxd's own copies of them.
	Secrets []string
	// Temp and Logs are emptied, keeping the directories themselves.
	Temp []string
	Logs []string
	// HostKeys is the directory whose ssh_host_* files go.
	HostKeys string
	// CloudInit are the paths that make the next boot the first boot of a new
	// machine: cloud-init's state and the machine-id files.
	CloudInit []string
	// Journal is true when the guest's journal is rotated and vacuumed.
	Journal bool
	// ImageRecord and Recipe are the two files a seal writes: what the image is,
	// and the script that would build it again.
	ImageRecord string
	Recipe      string
	// MountInfo is where the guest lists its mounts (/proc/self/mountinfo): the
	// mounts that are not on the disk are the node's, and the units systemd was
	// given for them belong to the instance the box was built in.
	MountInfo string
	// SystemdUnits is where the guest's own units live (/etc/systemd/system).
	SystemdUnits string
	// Fstab is the guest's mount table. The box's node mounts are written into it
	// by the instance the box runs in, and a baked image must not carry them.
	Fstab string
}

// sealPaths fills the defaults of the guest and the configured overrides.
func (c DaemonConfig) sealPaths() SealPaths {
	p := SealPaths{
		Workspace:    DefaultWorkspaceRoot,
		Temp:         []string{"/tmp", "/var/tmp"},
		Logs:         []string{"/var/log"},
		HostKeys:     "/etc/ssh",
		CloudInit:    []string{"/var/lib/cloud/instances", "/var/lib/cloud/instance", "/var/lib/dbus/machine-id"},
		Journal:      true,
		Secrets:      c.secretsDirs(),
		ImageRecord:  imageRecordPath,
		Recipe:       recipePath,
		MountInfo:    "/proc/self/mountinfo",
		SystemdUnits: "/etc/systemd/system",
		Fstab:        "/etc/fstab",
	}
	if c.StateDir != "" {
		p.Homes = append(p.Homes, filepath.Join(c.StateDir, "homes"))
	}
	p.Homes = append(p.Homes, homesOf("/root", "/home/*")...)
	if o := c.Seal; o != nil {
		if o.Workspace != "" {
			p.Workspace = o.Workspace
		}
		if o.Homes != nil {
			p.Homes = o.Homes
		}
		if o.Secrets != nil {
			p.Secrets = o.Secrets
		}
		if o.Temp != nil {
			p.Temp = o.Temp
		}
		if o.Logs != nil {
			p.Logs = o.Logs
		}
		if o.HostKeys != "" {
			p.HostKeys = o.HostKeys
		}
		if o.CloudInit != nil {
			p.CloudInit = o.CloudInit
		}
		if o.ImageRecord != "" {
			p.ImageRecord = o.ImageRecord
		}
		if o.Recipe != "" {
			p.Recipe = o.Recipe
		}
		if o.MountInfo != "" {
			p.MountInfo = o.MountInfo
		}
		if o.SystemdUnits != "" {
			p.SystemdUnits = o.SystemdUnits
		}
		if o.Fstab != "" {
			p.Fstab = o.Fstab
		}
		p.Journal = o.Journal
	}
	return p
}

// homesOf returns every home directory that exists among the given ones.
func homesOf(roots ...string) []string {
	var out []string
	for _, root := range roots {
		switch {
		case strings.HasSuffix(root, "/*"):
			// A directory of homes (/home/*): every subdirectory is one.
			ents, err := os.ReadDir(strings.TrimSuffix(root, "/*"))
			if err != nil {
				continue
			}
			for _, e := range ents {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
					out = append(out, filepath.Join(strings.TrimSuffix(root, "/*"), e.Name()))
				}
			}
		default:
			if st, err := os.Stat(root); err == nil && st.IsDir() {
				out = append(out, root)
			}
		}
	}
	sort.Strings(out)
	return out
}

// secretsDirs are the directories a seal reads the credentials out of: the
// driver's secrets directory, and boxd's own copies of what it derives from them
// (which live on the tmpfs, and are therefore not on the disk to be found).
func (c DaemonConfig) secretsDirs() []string {
	out := []string{c.SecretsDir}
	if d := c.executorsDir(); d != "" {
		out = append(out, d)
	}
	return out
}

// Seal removes what belongs to one box and proves that what is left is an image.
//
// say receives every progress line; it may be nil. The lines also go to the
// journal, which is where a person watches a long seal from the task log.
//
// A seal is only for a guest that is about to become an image, so it refuses one
// that has the box's credentials: those are the files of the box's own work, and
// an image must never be made of a machine that holds them. `box bake` never runs
// into this, because it seals a clone that mounts empty stand-ins where a box has
// its credentials.
func (m *Manager) Seal(req SealRequest, say func(string)) (SealResult, error) {
	if say == nil {
		say = func(string) {}
	}
	if err := m.credentialsMounted(); err != nil {
		return SealResult{Steps: []string{}}, err
	}
	s := &sealer{m: m, cfg: m.cfg, paths: m.cfg.sealPaths(), say: say, res: SealResult{Steps: []string{}}}
	return s.run(req)
}

// sealer is one seal in progress.
type sealer struct {
	m     *Manager
	cfg   DaemonConfig
	paths SealPaths
	say   func(string)
	res   SealResult
	// zeroLimit bounds the zero fill that replaces fstrim, and zeroPath is the
	// directory it fills. zeroLimit is 0 for a real seal, which writes until the
	// filesystem is full; a test sets both so that it does not fill the machine
	// it runs on.
	zeroLimit int64
	zeroPath  string
}

// line reports one step of the seal: to the caller, and to the journal.
func (s *sealer) line(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	s.res.Steps = append(s.res.Steps, text)
	s.say(text)
	s.cfg.logf("seal: %s", text)
}

// fail wraps a step's failure. By the time a step runs the seal has already
// deleted something, and nothing it deletes comes back: the message says so, and
// that the machine a bake seals is a clone of the box.
func (s *sealer) fail(what string, err error) (SealResult, error) {
	return s.res, Errorf(CodeInternal,
		"the seal stopped at %s: %v. What it deleted is gone: a seal empties the machine it runs in, which for a "+
			"bake is a clone of the box and never the box itself. Fix what is left and bake again", what, err)
}

// run is the seal: refuse, close, delete, prove, trim, record.
func (s *sealer) run(req SealRequest) (SealResult, error) {
	if err := s.m.sealGate(); err != nil {
		return s.res, err
	}
	record, err := sealRecord(req, s.cfg.now())
	if err != nil {
		return s.res, err
	}
	s.res.Image = record
	if err := s.writeRecipe(req.Recipe); err != nil {
		return s.fail("writing the recipe", err)
	}
	s.line("closing the box's sessions and stopping its executors")
	closed := s.m.closeAllForSeal()
	derived, err := s.m.forgetExecutorConfig()
	if err != nil {
		return s.fail("removing the executor configuration boxd derived", err)
	}
	s.line("closed %d session(s); no executor runs in the box now; removed %d file(s) of the executor "+
		"configuration boxd derived from the credentials", closed, derived)

	for _, step := range []struct {
		what string
		run  func() error
	}{
		{"the workspace and the preparation", s.removeWorkspace},
		{"the sessions and their transcripts", s.removeSessions},
		{"the executor homes and the user homes", s.removeHomeState},
		{"the temporary directories", s.removeTemp},
		{"the journal and the logs", s.removeLogs},
		{"the mount points of the node", s.removeInstanceMounts},
		{"the SSH host keys", s.removeHostKeys},
		{"the cloud-init state and the machine id", s.removeMachineIdentity},
	} {
		if err := step.run(); err != nil {
			return s.fail("removing "+step.what, err)
		}
	}

	if err := s.prove(); err != nil {
		return s.res, err
	}
	if err := s.trim(); err != nil {
		return s.fail("trimming the free space", err)
	}
	if err := s.writeRecord(record); err != nil {
		return s.fail("writing "+s.paths.ImageRecord, err)
	}
	s.line("the image is sealed: this box is now the image %s (%s)", record.Name, dashOr(record.Version, "no version"))
	return s.res, nil
}

// credentialsMounted refuses a seal in a guest that has the box's credentials
// mounted.
//
// The credentials are the driver's files on the host's tmpfs, mounted into the
// guest at the secrets directory: a guest that has them is a box somebody works
// in, not an image about to be made. A bake's clone mounts empty stand-ins there,
// so the seal of a clone always passes this check.
func (m *Manager) credentialsMounted() error {
	dir := strings.TrimSpace(m.cfg.SecretsDir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return nil // no directory, or an empty one: nothing is mounted
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = append(names[:5], fmt.Sprintf("and %d more", len(names)-5))
	}
	return Errorf(CodeInvalid,
		"this guest has the box's credentials mounted in %s (%s), and a seal is only for a guest that is about to "+
			"become an image. Bake the box instead, which seals a clone with empty stand-ins for the credentials "+
			"(docs/guest-image.md)", dir, strings.Join(names, ", "))
}

// SealStatus reports whether a seal would run in this box now, without changing
// anything: it is the seal's own gate, asked before the box is stopped.
//
// `box bake` asks it through `boxctl info` (the guest is the only place a turn
// or a preparation is visible), so a busy box is refused with no stop at all.
func (m *Manager) SealStatus() SealStatus {
	if err := m.sealGate(); err != nil {
		reason := err.Error()
		if e, ok := err.(*Error); ok {
			reason = e.Message
		}
		return SealStatus{OK: false, Reason: reason}
	}
	return SealStatus{OK: true}
}

// sealGate refuses a seal that would race a turn or a preparation: both are work
// that a seal deletes out from under itself.
func (m *Manager) sealGate() error {
	if st := m.prep.State(); st.State == PrepareRunning {
		what := st.Step
		if st.LastLine != "" {
			what += ": " + st.LastLine
		}
		return Errorf(CodeBusy, "the box is being prepared (%s): a seal removes the sources and their state, "+
			"so stop the preparation first (`box prepare <box> --cancel`)", what)
	}
	for _, meta := range m.List() {
		s := m.lookupSession(meta.ID)
		if s == nil {
			continue
		}
		s.mu.Lock()
		busy := s.busy
		turn := 0
		if s.cur != nil {
			turn = s.cur.Turn.Turn
		}
		s.mu.Unlock()
		if busy {
			what := "a turn"
			if turn > 0 {
				what = fmt.Sprintf("turn %d", turn)
			}
			return Errorf(CodeBusy, "session %s is running %s: a seal removes the sessions and their transcripts, "+
				"so cancel it first (`box cancel <box> %s`)", meta.ID, what, meta.ID)
		}
	}
	return nil
}

// closeAllForSeal closes every session, stops the executors, and makes the
// daemon forget both.
//
// The forgetting is the point: a session left in the manager would be written
// back into the state directory by the next persist (a close, or boxd's own
// shutdown when the box is stopped), and a sealed image would carry the session
// list the seal just deleted. A sealed boxd starts no session either.
func (m *Manager) closeAllForSeal() int {
	closed := 0
	for _, meta := range m.List() {
		if meta.State == StateClosed {
			continue
		}
		if err := m.Close(meta.ID); err != nil {
			m.cfg.logf("seal: closing session %s: %v", meta.ID, err)
			continue
		}
		closed++
	}
	m.mu.Lock()
	m.sessions = map[string]*session{}
	m.order = nil
	m.sealed = true
	m.mu.Unlock()
	return closed
}

// forgetExecutorConfig removes the copies boxd derived from the credentials: the
// executor directories in the runtime directory (a tmpfs), where an agent's own
// settings and the credentials it was started with live.
//
// The driver's own copies are the read-only mount of /run/box/secrets and stay
// where they are; these are boxd's, and a sealed box keeps neither. They are on
// the tmpfs, so no image carries them -- but guest/no-secrets.sh asks a sealed box
// for them and fails the seal when it finds one, which is what makes the proof
// mean something: boxd forgot the credential, and did not just leave it in a
// place the image happens not to cover.
func (m *Manager) forgetExecutorConfig() (int, error) {
	dir := m.cfg.executorsDir()
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				removed++
			}
			return nil
		})
		if err != nil {
			return removed, err
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// sealRecord builds the image record a seal writes, from the request.
func sealRecord(req SealRequest, now time.Time) (ImageRecord, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ImageRecord{}, Errorf(CodeInvalid, "a seal needs a name for the image (--name)")
	}
	if !imageNamePattern.MatchString(name) {
		return ImageRecord{}, Errorf(CodeInvalid, "the image name %q is not usable: use lower-case letters, digits and dashes", name)
	}
	version := strings.TrimSpace(req.Version)
	if version != "" && !imageVersionPattern.MatchString(version) {
		return ImageRecord{}, Errorf(CodeInvalid, "the image version %q is not usable: use lower-case letters, digits, dots and dashes", version)
	}
	arch := strings.TrimSpace(req.Arch)
	if arch != "" && arch != "amd64" && arch != "arm64" {
		return ImageRecord{}, Errorf(CodeInvalid, "the architecture %q is not usable: amd64 or arm64", arch)
	}
	recipe, err := normalizeRecipe(req.Recipe)
	if err != nil {
		return ImageRecord{}, err
	}
	return ImageRecord{
		Schema:   1,
		Version:  version,
		Name:     name,
		Arch:     arch,
		Parent:   strings.TrimSpace(req.Parent),
		Platform: "mount",
		Recipe:   recipe,
		SealedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

// normalizeRecipe checks the recipe an image carries and returns it in its
// canonical base64 form: a script of at most RecipeLimit bytes, or "" when the
// image has none.
func normalizeRecipe(encoded string) (string, error) {
	if strings.TrimSpace(encoded) == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", Errorf(CodeInvalid, "the recipe is not base64: %v", err)
	}
	if len(data) > RecipeLimit {
		return "", Errorf(CodeInvalid, "the recipe is %d bytes, more than the %d an image's record carries", len(data), RecipeLimit)
	}
	if len(data) == 0 {
		return "", nil
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// writeRecipe puts the recipe at /etc/box/recipe.sh, where a rebuild replays it,
// and keeps it for the image record.
func (s *sealer) writeRecipe(encoded string) error {
	if strings.TrimSpace(encoded) == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Errorf(CodeInvalid, "the recipe is not base64: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.paths.Recipe), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.paths.Recipe, data, 0o755); err != nil {
		return err
	}
	// boxd runs as root, and the umask of a service may take the bits away.
	if err := os.Chmod(s.paths.Recipe, 0o755); err != nil {
		return err
	}
	s.line("wrote the recipe to %s (%d bytes)", s.paths.Recipe, len(data))
	return nil
}

// writeRecord writes the image record, and reads it back: a record that did not
// arrive is an image that does not know what it is.
func (s *sealer) writeRecord(record ImageRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.paths.ImageRecord), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.paths.ImageRecord + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.paths.ImageRecord); err != nil {
		return err
	}
	back, err := os.ReadFile(s.paths.ImageRecord)
	if err != nil {
		return err
	}
	var got ImageRecord
	if err := json.Unmarshal(back, &got); err != nil {
		return fmt.Errorf("the record read back is not JSON: %w", err)
	}
	if got.Name != record.Name || got.SealedAt != record.SealedAt {
		return fmt.Errorf("the record read back names %q sealed at %q, want %q at %q",
			got.Name, got.SealedAt, record.Name, record.SealedAt)
	}
	s.line("recorded the image in %s", s.paths.ImageRecord)
	return nil
}

// removeWorkspace deletes the workspace root and the sources and the state of the
// box's last preparation: what a session worked in is code, and code does not
// belong in an image.
func (s *sealer) removeWorkspace() error {
	if err := s.removeTree(s.paths.Workspace, "the workspace"); err != nil {
		return err
	}
	state := filepath.Join(s.cfg.StateDir, "prepare.json")
	dest := recordedPrepareDest(state)
	if dest != "" {
		// The sources were renamed into place from "<dest>.partial", and a
		// preparation that failed may have left that behind.
		for _, path := range []string{dest + ".partial", dest} {
			if err := s.removeTree(path, "the preparation's sources"); err != nil {
				return err
			}
		}
	}
	return s.removeTree(state, "the preparation's state")
}

// recordedPrepareDest reads the destination of the box's last preparation, which
// is where its sources are.
func recordedPrepareDest(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var st PrepareState
	if json.Unmarshal(b, &st) != nil {
		return ""
	}
	return st.Dest
}

// removeSessions deletes the session store and every session's directory: the
// transcripts of a session are the work of one project, and they name it.
func (s *sealer) removeSessions() error {
	for _, path := range []string{
		filepath.Join(s.cfg.StateDir, "state.json"),
		filepath.Join(s.cfg.StateDir, "sessions"),
		filepath.Join(s.cfg.StateDir, "boxd.pid"),
	} {
		if err := s.removeTree(path, "the sessions"); err != nil {
			return err
		}
	}
	return nil
}

// removeTree deletes path and everything below it, and reports what it freed. A
// path that is not there is not a failure: a seal runs again on a box it partly
// cleaned.
func (s *sealer) removeTree(path, what string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == "/" {
		return fmt.Errorf("refusing to delete %q", path)
	}
	if _, err := os.Lstat(clean); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	size := treeSize(clean)
	if err := os.RemoveAll(clean); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	s.line("%s: deleted %s (%s)", what, clean, formatSize(size))
	return nil
}

// removeChildren empties a directory and keeps the directory itself: /tmp, /var,
// /var/log and the cloud-init directories have to be there for the next boot.
func (s *sealer) removeChildren(dir, what string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	var freed int64
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		size := treeSize(path)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		freed += size
		removed++
	}
	if removed > 0 {
		s.line("%s: emptied %s (%d entr%s, %s)", what, dir, removed, plural(removed, "y", "ies"), formatSize(freed))
	}
	return nil
}

// removeHomeState cleans the executor homes and every user's home.
//
// The homes stay: they hold what an image is for -- the toolchains, the package
// caches and the language environments an agent installed, which is the expensive
// half of a project's environment. What goes is everything that belongs to one
// session or one credential: a file named like one of the box's credentials, the
// directory an executor keeps its own sessions and transcripts in, the shell
// histories, the SSH keys, the stored git and npm credentials, and the git
// credential helper that would find them again.
func (s *sealer) removeHomeState() error {
	names, err := secretFileNames(s.paths.Secrets)
	if err != nil {
		return err
	}
	for _, home := range s.paths.Homes {
		if err := s.cleanHome(home, names); err != nil {
			return fmt.Errorf("%s: %w", home, err)
		}
	}
	return nil
}

// cleanHome removes one home's session and credential state.
func (s *sealer) cleanHome(home string, secretNames map[string]bool) error {
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	removed := 0
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(home, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if isExecutorStateDir(rel) {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				removed++
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		switch {
		case base == ".npmrc":
			return stripNPMTokens(path)
		case base == ".gitconfig" || rel == ".config/git/config":
			return stripGitCredentialHelpers(path)
		}
		remove := secretNames[base] || shellHistory(base) ||
			base == ".git-credentials" || base == ".netrc" ||
			(strings.HasPrefix(rel, ".ssh/") && sshSecret(base))
		if !remove {
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if !d.IsDir() {
			removed++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if removed > 0 {
		s.line("the home %s: removed %d file(s) and directory state", home, removed)
	}
	return nil
}

// executorStateDirs are the directories an executor keeps its own sessions,
// transcripts and logs in, relative to its home. They are the ones the adapters
// in this daemon write (exec.go), and the ones guest/no-secrets.sh refuses to
// find in an image.
var executorStateDirs = []string{
	".prime",
	".claude",
	".config/opencode",
	".config/prime",
	".local/share/opencode",
	".local/share/prime",
	".cache/opencode",
}

// isExecutorStateDir reports whether rel names an executor's own state.
func isExecutorStateDir(rel string) bool {
	for _, dir := range executorStateDirs {
		if rel == dir {
			return true
		}
	}
	return false
}

// shellHistories are the files the shells a box runs write what was typed into.
var shellHistories = map[string]bool{
	".bash_history":      true,
	".zsh_history":       true,
	".sh_history":        true,
	".ash_history":       true,
	".python_history":    true,
	".node_repl_history": true,
	".psql_history":      true,
	".mysql_history":     true,
	".sqlite_history":    true,
	".lesshst":           true,
	".wget-hsts":         true,
	".viminfo":           true,
}

// shellHistory reports whether name is a history file, including the ones the
// shells keep in a subdirectory (fish).
func shellHistory(name string) bool {
	if shellHistories[name] {
		return true
	}
	return name == "fish_history" || name == "history.db"
}

// sshSecret reports whether name below an .ssh directory is a key, an authorized
// key or a known host: none of them belongs to another machine's image.
func sshSecret(name string) bool {
	switch {
	case name == "authorized_keys" || name == "authorized_keys2":
		return true
	case strings.HasPrefix(name, "id_") && !strings.HasSuffix(name, ".pub"):
		return true
	case strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key"):
		return true
	}
	return false
}

// secretFileNames lists the base names of every file in the secrets directories.
// A file of that name anywhere in a home is a copy of a credential.
func secretFileNames(dirs []string) (map[string]bool, error) {
	names := map[string]bool{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) || os.IsPermission(err) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			names[d.Name()] = true
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading the credentials in %s: %w", dir, err)
		}
	}
	return names, nil
}

// stripNPMTokens removes the lines of an .npmrc that carry a token, and leaves
// the rest (a registry, a proxy) where it is.
func stripNPMTokens(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var kept []string
	dropped := 0
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(trimmed, "_authtoken") || strings.Contains(trimmed, "_auth=") ||
			strings.Contains(trimmed, "_password") || strings.Contains(trimmed, "token=") {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped == 0 {
		return nil
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		return err
	}
	return nil
}

// stripGitCredentialHelpers removes the credential helper settings of a git
// configuration: a helper is a way to find the credential a seal just deleted,
// through a file that is not itself named like one.
func stripGitCredentialHelpers(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var kept []string
	section := ""
	dropped := 0
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section = strings.ToLower(strings.Trim(trimmed, "[]"))
		}
		if strings.HasPrefix(section, "credential") && strings.HasPrefix(strings.ToLower(trimmed), "helper") {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped == 0 {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o600)
}

// removeInstanceMounts deletes what connects the node's mounts to the instance the
// box was built in: their lines in /etc/fstab, and a systemd mount unit that names
// one of them.
//
// This is the difference between an image that boots and one that does not. Lima
// writes the box's mounts into /etc/fstab, and systemd turns every fstab line into
// a mount unit that local-fs.target starts at boot. A baked image carrying them
// would bring up mounts of the *previous* instance -- the 9p tag names the host
// paths of that box -- and boxd, whose unit requires those mounts, would be refused
// before it ever ran: the box looks dead, and nothing in its log says why. The node
// writes the mounts of the new instance again on the next boot, with its own tag.
func (s *sealer) removeInstanceMounts() error {
	points := instanceMountPoints(s.paths.MountInfo)
	if len(points) == 0 {
		return nil
	}
	removed, err := removeFstabMounts(s.paths.Fstab, points)
	if err != nil {
		return err
	}
	if removed > 0 {
		s.line("the mount points of the box's instance: removed %d line(s) from %s", removed, s.paths.Fstab)
	}
	return s.removeInstanceMountUnits(points)
}

// removeFstabMounts deletes the fstab lines that mount one of points, and returns
// how many it deleted. Blank lines and comments stay, and so does every other mount
// the image carries (its own root, a swap file).
func removeFstabMounts(path string, points []string) (int, error) {
	if path == "" {
		return 0, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	want := make(map[string]bool, len(points))
	for _, point := range points {
		want[point] = true
	}
	var kept []string
	removed := 0
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && want[fields[1]] {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644)
}

// removeInstanceMountUnits deletes the systemd mount units of the mounts the node
// makes into this box.
//
// They are the units of the instance the box was built in: Lima writes one per
// mount on the box's first boot, with the mount point and the tag of that
// instance, and they stay on the disk. A baked image that carried them would come
// up with a mount that cannot work, and boxd -- whose unit requires those mounts --
// would be refused before it ever ran; the box would look dead with nothing in its
// log that says why. The node's mounts are not on the disk, so the units go with
// the instance, not with the image.
func (s *sealer) removeInstanceMountUnits(points []string) error {
	if s.paths.SystemdUnits == "" || len(points) == 0 {
		return nil
	}
	entries, err := os.ReadDir(s.paths.SystemdUnits)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".mount") {
			continue
		}
		path := filepath.Join(s.paths.SystemdUnits, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !unitMountsAny(data, points) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		// Every unit that wants or requires it names it by a link in its own
		// directory.
		links, _ := filepath.Glob(filepath.Join(s.paths.SystemdUnits, "*.wants", entry.Name()))
		more, _ := filepath.Glob(filepath.Join(s.paths.SystemdUnits, "*.requires", entry.Name()))
		for _, link := range append(links, more...) {
			_ = os.Remove(link)
		}
		removed++
	}
	if removed == 0 {
		return nil
	}
	s.line("the mount units of the box's own instance: removed %d from %s", removed, s.paths.SystemdUnits)
	// The running systemd still holds them: a reload is what makes the next start
	// of the box use the units the new instance writes.
	if name, err := exec.LookPath("systemctl"); err == nil {
		if out, err := exec.Command(name, "daemon-reload").CombinedOutput(); err != nil {
			s.cfg.logf("seal: systemctl daemon-reload: %v: %s", err, oneLine(string(out), 200))
		}
	}
	return nil
}

// instanceMountPoints reads the mount points of the mounts that are not on the box
// disk: the node shares them into the guest (9p, or virtiofs under vz), and the
// credentials are one of them.
func instanceMountPoints(path string) []string {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		// id parent major:minor root mountpoint options [optional...] - fstype
		// source super-options
		sep := 0
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		if sep == 0 || sep+1 >= len(fields) || len(fields) < 5 {
			continue
		}
		switch fields[sep+1] {
		case "9p", "virtiofs":
			out = append(out, unescapeMountPath(fields[4]))
		}
	}
	return out
}

// unescapeMountPath turns the octal escapes of mountinfo back into a path.
func unescapeMountPath(path string) string {
	if !strings.Contains(path, "\\") {
		return path
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] == '\\' && i+3 < len(path) {
			code := path[i+1 : i+4]
			if n, err := strconv.ParseUint(code, 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

// unitMountsAny reports whether a systemd mount unit mounts one of the points.
func unitMountsAny(unit []byte, points []string) bool {
	for _, line := range strings.Split(string(unit), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Where=") {
			continue
		}
		where := strings.TrimSpace(strings.TrimPrefix(line, "Where="))
		for _, point := range points {
			if where == point {
				return true
			}
		}
	}
	return false
}

// removeTemp empties the temporary directories.
func (s *sealer) removeTemp() error {
	for _, dir := range s.paths.Temp {
		if err := s.removeChildren(dir, "temporary files"); err != nil {
			return err
		}
	}
	return nil
}

// removeLogs rotates and vacuums the journal and empties the log directories.
func (s *sealer) removeLogs() error {
	if s.paths.Journal {
		for _, args := range [][]string{{"--rotate"}, {"--vacuum-time=1s"}} {
			name := s.cfg.journalctlBin()
			if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
				s.cfg.logf("seal: %s %s: %v: %s", name, strings.Join(args, " "), err, oneLine(string(out), 200))
			}
		}
	}
	for _, dir := range s.paths.Logs {
		if err := s.removeChildren(dir, "the logs"); err != nil {
			return err
		}
	}
	return nil
}

// removeHostKeys deletes the SSH host keys: the next boot generates its own, and
// an image that carried them would let anyone impersonate every box made from it.
func (s *sealer) removeHostKeys() error {
	if s.paths.HostKeys == "" {
		return nil
	}
	entries, err := os.ReadDir(s.paths.HostKeys)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	removed := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "ssh_host_") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.paths.HostKeys, entry.Name())); err != nil {
			return err
		}
		removed++
	}
	if removed > 0 {
		s.line("the SSH host keys: deleted %d file(s) from %s", removed, s.paths.HostKeys)
	}
	return nil
}

// removeMachineIdentity makes the next boot the first boot of a new machine: no
// cloud-init state, and no machine id from the build.
func (s *sealer) removeMachineIdentity() error {
	if name, err := exec.LookPath("cloud-init"); err == nil {
		if out, err := exec.Command(name, "clean", "--logs", "--machine-id").CombinedOutput(); err != nil {
			s.cfg.logf("seal: cloud-init clean: %v: %s", err, oneLine(string(out), 200))
		} else {
			s.line("cloud-init state: cleaned (the next boot is a first boot)")
		}
	}
	for _, path := range s.paths.CloudInit {
		if filepath.Base(path) == "machine-id" {
			if err := wipeMachineID(path); err != nil {
				return err
			}
			continue
		}
		if err := s.removeChildren(path, "cloud-init state"); err != nil {
			return err
		}
	}
	return nil
}

// wipeMachineID empties a machine-id file, or removes it when it is not a file
// this can truncate. An empty one is what a distribution's boot generates.
func wipeMachineID(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// prove runs the check that decides whether the box may become an image: the
// bundle's own guest/no-secrets.sh. It is the proof the seal keeps.
func (s *sealer) prove() error {
	return s.proveNoSecrets()
}

// proveNoSecrets runs the bundle's guest/no-secrets.sh: the same check the image
// build runs before it captures a disk, and the one an operator runs in a box.
func (s *sealer) proveNoSecrets() error {
	script := filepath.Join(s.cfg.platformDir(), "lib", "no-secrets.sh")
	if st, err := os.Stat(script); err != nil || !st.Mode().IsRegular() {
		return Errorf(CodeInternal,
			"the seal cannot prove the image has no credential: the platform bundle at %s carries no lib/no-secrets.sh. "+
				"A bundle built by images/platform.sh carries it (docs/guest-image.md); install one on this node and bake again",
			s.cfg.platformDir())
	}
	out, err := exec.Command("bash", script, s.res.Image.Version).CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}
		s.say("no-secrets: " + line)
	}
	if err != nil {
		var failures []string
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "FAIL") {
				failures = append(failures, strings.TrimSpace(line))
			}
		}
		if len(failures) == 0 {
			failures = []string{oneLine(string(out), 300)}
		}
		if len(failures) > 5 {
			failures = append(failures[:5], fmt.Sprintf("and %d more", len(failures)-5))
		}
		return Errorf(CodeInternal,
			"guest/no-secrets.sh failed in this box: %s. A seal empties the machine it runs in, which for a bake is "+
				"a clone of the box and never the box itself: remove what it names in the box and bake again",
			strings.Join(failures, "; "))
	}
	s.line("guest/no-secrets.sh: %d check(s) passed", countLines(out))
	return nil
}

// trim drops the blocks the seal freed, so that a qcow2 built from this disk does
// not carry the data the seal deleted.
func (s *sealer) trim() error {
	name := s.cfg.fstrimBin()
	out, err := exec.Command(name, "-a").CombinedOutput()
	if err == nil {
		s.line("trimmed the free space (fstrim -a)")
	} else {
		s.cfg.logf("seal: %s -a: %v: %s", name, err, oneLine(string(out), 200))
		s.line("fstrim is not available on this guest (%s); zero-filling the free space instead", oneLine(err.Error(), 120))
		if err := s.zeroFreeSpace(); err != nil {
			return err
		}
	}
	syscall.Sync()
	s.line("synced the disk")
	return nil
}

// zeroFreeSpace writes zeros into a file until the filesystem is full, which is
// what makes the free blocks of a disk read as zero in an image. The file is
// deleted again; the space it wrote stays empty and zeroed.
//
// zeroLimit bounds the work for a test; a real seal writes until the filesystem
// answers "no space left", which is exactly the free space.
func (s *sealer) zeroFreeSpace() error {
	dir := s.zeroPath
	if dir == "" {
		dir = "/"
	}
	limit := s.zeroLimit
	path := filepath.Join(dir, "zero.fill")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	chunk := make([]byte, 1<<20)
	var written int64
	for {
		size := int64(len(chunk))
		if limit > 0 && written+size > limit {
			size = limit - written
		}
		if size <= 0 {
			break
		}
		n, werr := f.Write(chunk[:size])
		written += int64(n)
		if werr != nil {
			// Out of space is the point of the exercise.
			break
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	s.line("zero-filled %s of free space", formatSize(written))
	return nil
}

// treeSize is what the files below path occupy, in bytes.
func treeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// countLines is the number of non-empty lines in b.
func countLines(b []byte) int {
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// formatSize renders a byte count the way the task log reads it.
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// plural picks the ending of a counted noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// dashOr renders a value, or a dash when it is empty.
func dashOr(value, empty string) string {
	if strings.TrimSpace(value) == "" {
		return empty
	}
	return value
}
