package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// A bake works on a clone of the box, never on the box itself.
//
// `limactl clone` copies a stopped instance -- measured with Lima 2.2.0: a full
// copy of the disk file, a new hostname, and no snapshot -- so the seal runs in a
// machine of its own and the box keeps its work: /work and the sessions are
// still there when the box starts again. The clone is started with the platform
// bundle mounted at /opt/box and two empty directories where boxd.service waits
// for its mounts, so the guest the seal empties never sees a credential. It is
// deleted by the bake that made it, whatever happens, and a clone a killed bake
// left behind is removed by the next bake of the same box (or reported as an
// orphan: its disk is not on a volume).
const (
	// BakeCloneInfix marks the instance name of a bake's clone:
	// <box id>-bake-<n>.
	BakeCloneInfix = "-bake-"
	// cloneMountsDir is the directory inside the clone's instance directory that
	// holds the two empty directories the clone mounts where a box has its task
	// files and its credentials. It lives in the clone's own directory, so
	// `limactl delete` removes it with the clone.
	cloneMountsDir = "box-bake-mounts"
	// boxctlInGuest is the boxctl of the platform bundle the clone mounts: the
	// same bundle boxd runs from, so the client and the daemon are one version.
	boxctlInGuest = PlatformMountPoint + "/bin/boxctl"
)

// BakeCloneName is the instance name of the n-th clone of a box.
func BakeCloneName(boxID string, n int) string {
	return fmt.Sprintf("%s%s%d", boxID, BakeCloneInfix, n)
}

// bakeClonePattern matches the instance name of a bake's clone of boxID.
func bakeClonePattern(boxID string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(boxID+BakeCloneInfix) + `([0-9]+)$`)
}

// BakeClones names the bake clones of a box that exist now, in name order.
//
// A bake that was killed leaves its clone behind; the next bake of the same box
// removes it before it makes its own, and until then the driver reports it as an
// orphan (its disk is not on a volume, which is all an orphan is there).
func (m *Manager) BakeClones(ctx context.Context, boxID string) ([]string, error) {
	insts, err := m.lima.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list lima instances: %w", err)
	}
	pattern := bakeClonePattern(boxID)
	var out []string
	for _, inst := range insts {
		if inst != nil && pattern.MatchString(inst.Name) {
			out = append(out, inst.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// NextBakeCloneName is the name of the next clone of a box: one above the
// highest number that exists, so a name is never reused while a clone of it
// still runs.
func NextBakeCloneName(boxID string, existing []string) string {
	pattern := bakeClonePattern(boxID)
	next := 1
	for _, name := range existing {
		match := pattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		if n, err := strconv.Atoi(match[1]); err == nil && n >= next {
			next = n + 1
		}
	}
	return BakeCloneName(boxID, next)
}

// BakeClone is the temporary Lima instance one bake seals.
type BakeClone struct {
	m      *Manager
	Name   string
	Source string
}

// BakeCloneOf names an existing bake clone of a box, which is how a leftover of a
// killed bake is removed.
func (m *Manager) BakeCloneOf(name string) *BakeClone { return &BakeClone{m: m, Name: name} }

// CloneForBake copies the stopped box source into a new instance called name,
// with only the platform bundle and two empty stand-in directories as mounts.
//
// platformDir is the unpacked platform bundle on this node (EnsurePlatform); the
// stand-ins are empty directories inside the clone's own directory, which is
// what boxd.service waits for at /run/box/secrets and /etc/box/local. The clone
// therefore starts the bundle's boxd without ever seeing the box's task files or
// its credentials.
func (m *Manager) CloneForBake(ctx context.Context, source, name, platformDir string) (*BakeClone, error) {
	if platformDir == "" {
		return nil, fmt.Errorf("a bake's clone needs the platform bundle to mount at %s", PlatformMountPoint)
	}
	if err := CheckSocketPath(m.opts.LimaHome, name); err != nil {
		return nil, fmt.Errorf("the clone of box %s cannot be named %q: %v", source, name, err)
	}
	if inst, err := m.lima.Get(ctx, name); err != nil {
		return nil, fmt.Errorf("list lima instances: %w", err)
	} else if inst != nil {
		return nil, fmt.Errorf("the lima instance %s already exists", name)
	}
	dir := m.lima.InstanceDir(name)
	standIn := filepath.Join(dir, cloneMountsDir)
	c := &BakeClone{m: m, Name: name, Source: source}
	// The source's mounts -- the box's task files and its credentials above all
	// -- must not reach the clone, so it is created without any mount and its own
	// are written into its configuration below. `limactl clone --mount-none` is
	// what removes them (measured with Lima 2.2.0); --mount or --mount-only
	// cannot say a guest path (they mount at the same path on both sides) and
	// cannot be combined with --mount-none.
	if _, stderr, err := m.lima.RunLong(ctx, "clone", "--tty=false", "--mount-none", source, name); err != nil {
		_ = c.Delete(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("clone box %s into %s: %w%s", source, name, err, suffix(stderr))
	}
	// The stand-ins go in after the clone: `limactl clone` refuses an instance
	// directory that already exists (measured with Lima 2.2.0), and the clone's
	// own directory is where they belong, so `limactl delete` removes them.
	// Each level belongs to the runtime user, which runs qemu: created in one go,
	// the parent would stay the driver's, and qemu could not enter it.
	if err := m.opts.Identity.EnsureDir(standIn, 0o700); err != nil {
		_ = c.Delete(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("create the stand-in mounts of the clone: %w", err)
	}
	for _, part := range []string{"local", "secrets"} {
		if err := m.opts.Identity.EnsureDir(filepath.Join(standIn, part), 0o700); err != nil {
			_ = c.Delete(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("create the stand-in mount %s of the clone: %w", part, err)
		}
	}
	if err := m.setCloneMounts(name, platformDir, standIn); err != nil {
		_ = c.Delete(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := c.verifyMounts(platformDir, standIn); err != nil {
		_ = c.Delete(context.WithoutCancel(ctx))
		return nil, err
	}
	if err := c.verifyDisk(); err != nil {
		_ = c.Delete(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}

// setCloneMounts writes the three mounts a bake's clone has into its
// configuration: the platform bundle at /opt/box, and two empty directories where
// boxd.service waits for the task files and the credentials.
//
// The document is edited, not re-rendered: Lima adds fields of its own to an
// instance's configuration (the ssh port, the image digest, the host resolver),
// and losing one of them breaks a boot.
func (m *Manager) setCloneMounts(name, platformDir, standIn string) error {
	path := m.lima.InstanceDir(name)
	doc, err := readLimaYAML(path)
	if err != nil {
		return fmt.Errorf("read the configuration of the clone %s: %w", name, err)
	}
	doc["mounts"] = []any{
		m.platformMountDocument(platformDir),
		m.standInMountDocument(filepath.Join(standIn, "secrets"), DefaultSecretsDirInGuest),
		m.standInMountDocument(filepath.Join(standIn, "local"), filepath.Join(defaultFilesDirInGuest, boxLocalDir)),
	}
	doc["mountType"] = m.mountType()
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("render the configuration of the clone %s: %w", name, err)
	}
	file := m.instanceLimaYAML(name)
	tmp := file + ".box-new"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write the configuration of the clone %s: %w", name, err)
	}
	if err := m.opts.Identity.Chown(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace the configuration of the clone %s: %w", name, err)
	}
	return nil
}

// standInMountDocument is a mount entry of an empty host directory: the clone
// mounts one where a box has its task files and its credentials, so boxd starts
// without ever seeing either.
func (m *Manager) standInMountDocument(location, mountPoint string) map[string]any {
	entry := map[string]any{
		"location":   location,
		"mountPoint": mountPoint,
		"writable":   false,
	}
	if m.opts.VMType == VMTypeQEMU {
		entry["9p"] = map[string]any{"cache": CacheNone}
	}
	return entry
}

// verifyMounts reads the clone's configuration back: the clone must mount the
// bundle and the two stand-ins, and nothing else. A mount of the box's own task
// files would put a credential in the machine the seal empties, which is the one
// thing a bake must never do.
func (c *BakeClone) verifyMounts(platformDir, standIn string) error {
	doc, err := readLimaYAML(c.m.lima.InstanceDir(c.Name))
	if err != nil {
		return fmt.Errorf("read the configuration of the clone %s: %w", c.Name, err)
	}
	want := map[string]string{
		PlatformMountPoint:                                 platformDir,
		DefaultSecretsDirInGuest:                           filepath.Join(standIn, "secrets"),
		filepath.Join(defaultFilesDirInGuest, boxLocalDir): filepath.Join(standIn, "local"),
	}
	mounts, _ := doc["mounts"].([]any)
	for _, raw := range mounts {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		point, _ := entry["mountPoint"].(string)
		location, _ := entry["location"].(string)
		wantLocation, ok := want[point]
		if !ok {
			return fmt.Errorf("the clone %s mounts %s at %s, which a bake's clone must not mount: it is only given "+
				"the platform bundle and two empty directories", c.Name, location, point)
		}
		if filepath.Clean(location) != filepath.Clean(wantLocation) {
			return fmt.Errorf("the clone %s mounts %s at %s; a bake's clone mounts %s there", c.Name, location, point, wantLocation)
		}
	}
	for point := range want {
		if !mountsPoint(mounts, point) {
			return fmt.Errorf("the clone %s does not mount %s, so boxd would wait for it and never start", c.Name, point)
		}
	}
	return nil
}

// mountsPoint reports whether one of the mounts of a lima configuration mounts
// point.
func mountsPoint(mounts []any, point string) bool {
	for _, raw := range mounts {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if at, _ := entry["mountPoint"].(string); filepath.Clean(at) == filepath.Clean(point) {
			return true
		}
	}
	return false
}

// verifyDisk checks that the clone has a disk file of its own.
//
// It is the one thing a bake must be sure of: `limactl clone` copies the disk of
// a stopped instance, but a clone whose disk turned out to be a symlink to the
// box's own volume would make the seal empty the box the owner is working in. The
// copy is made here in that case, before anything starts.
func (c *BakeClone) verifyDisk() error {
	disk := c.Disk()
	fi, err := os.Lstat(disk)
	if err != nil {
		return fmt.Errorf("the clone %s has no disk: %w", c.Name, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("the disk of the clone %s is not a regular file", c.Name)
		}
		return nil
	}
	target, err := os.Readlink(disk)
	if err != nil {
		return fmt.Errorf("read the disk of the clone %s: %w", c.Name, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(disk), target)
	}
	tmp := disk + ".box-copy"
	if out, err := c.m.childCommand(context.Background(), "cp", append(sparseCopyFlags(), target, tmp)...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("copy the disk of box %s into the clone %s: %w: %s", c.Source, c.Name, err, strings.TrimSpace(string(out)))
	}
	if err := c.m.opts.Identity.Chown(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, disk); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("put the disk of the clone %s in place: %w", c.Name, err)
	}
	return nil
}

// readLimaYAML reads one instance's configuration as a document.
func readLimaYAML(instanceDir string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(instanceDir, "lima.yaml"))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not YAML: %w", filepath.Join(instanceDir, "lima.yaml"), err)
	}
	return doc, nil
}

// Disk is the disk file of the clone: the copy of the box's disk that the seal
// empties and the bake compresses.
func (c *BakeClone) Disk() string { return c.m.lima.DiskPath(c.Name) }

// InstanceDir is where the clone lives.
func (c *BakeClone) InstanceDir() string { return c.m.lima.InstanceDir(c.Name) }

// Start boots the clone. limactl start waits for the guest to answer, so this
// returns with the clone's boxd about to be started (see WaitBoxd).
func (c *BakeClone) Start(ctx context.Context) error {
	if _, stderr, err := c.m.lima.RunLong(ctx, "start", "--tty=false", c.Name); err != nil {
		return fmt.Errorf("start the clone %s: %w%s", c.Name, err, suffix(stderr))
	}
	return nil
}

// WaitBoxd waits until boxd answers in the clone, and reports the last thing it
// said when it does not.
//
// The clone boots like any box: Lima starts it, and boxd's own unit waits for the
// mounts the clone has, then starts the daemon. A seal needs the daemon, so the
// bake waits for it instead of failing on the first connection error.
func (c *BakeClone) WaitBoxd(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		if _, err := c.boxctl(ctx, []string{"info"}, nil); err == nil {
			return nil
		} else {
			last = oneLineErr(err.Error())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boxd does not answer in the clone %s after %s: %s", c.Name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// CloneSeal is what a seal of a clone makes of it: the image the clone's disk
// becomes.
type CloneSeal struct {
	Name    string
	Version string
	Parent  string
	Arch    string
	// Recipe is the script that makes this environment from the parent image; it
	// goes to boxctl on stdin, so no file of it is written in the guest.
	Recipe []byte
}

// Seal runs boxctl seal in the clone and reports every step it prints.
//
// boxctl answers a stream of JSON documents -- one line per step, then the
// record or an error -- and exits non-zero on a failure it wrote on that stream,
// so the stream is read before the exit code is: its error document is the
// sentence a person reads.
func (c *BakeClone) Seal(ctx context.Context, opts CloneSeal, say func(string)) error {
	args := []string{"seal", "--name", opts.Name}
	for _, part := range []struct{ flag, value string }{
		{"--version", opts.Version}, {"--parent", opts.Parent}, {"--arch", opts.Arch},
	} {
		if strings.TrimSpace(part.value) != "" {
			args = append(args, part.flag, part.value)
		}
	}
	var stdin []byte
	if len(opts.Recipe) > 0 {
		args = append(args, "--recipe", "-")
		stdin = opts.Recipe
	}
	out, runErr := c.boxctl(ctx, args, stdin)
	var sealErr error
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var doc struct {
			Line  string    `json:"line"`
			Seal  *struct{} `json:"seal"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			continue
		}
		switch {
		case doc.Error != nil && doc.Error.Message != "":
			sealErr = fmt.Errorf("%s", doc.Error.Message)
		case doc.Line != "" && say != nil:
			say(doc.Line)
		}
	}
	if sealErr != nil {
		return sealErr
	}
	return runErr
}

// boxctl runs one boxctl command in the clone and returns its stdout, with the
// failure to run it or the non-zero exit it reported.
func (c *BakeClone) boxctl(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	command := append([]string{"sudo", "-n", boxctlInGuest}, args...)
	cmd := c.m.lima.Command(ctx, append([]string{"shell", c.Name, "--"}, command...)...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := oneLineErr(stderr.String())
		if detail == "" {
			detail = oneLineErr(stdout.String())
		}
		return stdout.Bytes(), fmt.Errorf("boxctl %s in the clone %s: %v: %s", args[0], c.Name, err, detail)
	}
	return stdout.Bytes(), nil
}

// oneLineErr keeps a message on one line: task events are read in a terminal.
func oneLineErr(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "\n"); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	return text
}

// Stop shuts the clone down, escalating to a hard kill after timeout.
func (c *BakeClone) Stop(ctx context.Context, timeout time.Duration) error {
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.m.lima.Stop(stopCtx, c.Name); err != nil {
		if ferr := c.m.lima.StopForce(ctx, c.Name); ferr != nil {
			return fmt.Errorf("stop the clone %s: %w (forced stop also failed: %v)", c.Name, err, ferr)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		inst, err := c.m.lima.Get(ctx, c.Name)
		if err != nil {
			return fmt.Errorf("list lima instances: %w", err)
		}
		if inst == nil || !inst.IsRunning() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the clone %s is still running after %s", c.Name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// sparseCopyFlags keeps a thin disk thin: GNU cp on Linux, and an APFS clone on
// macOS, whose cp has no --sparse.
func sparseCopyFlags() []string {
	if runtime.GOOS == "darwin" {
		return []string{"-c"}
	}
	return []string{"--sparse=always"}
}

// Delete removes the clone: the instance directory with the copy of the disk
// and the stand-in mounts, so nothing of the box stays on the node.
//
// It is called with a context of its own (not the bake's), because it runs on the
// way out, also when the bake failed or was stopped.
func (c *BakeClone) Delete(ctx context.Context) error {
	delCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// A clone that is already stopped reports that as an error; the delete below
	// is what matters.
	_ = c.m.lima.StopForce(delCtx, c.Name)
	delErr := c.m.lima.Delete(delCtx, c.Name)
	dir := c.m.lima.InstanceDir(c.Name)
	if _, err := os.Lstat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil // gone, which is the point
		}
		return fmt.Errorf("look at the clone's directory %s: %w", dir, err)
	}
	// limactl refuses to delete an instance directory whose lima.yaml it cannot
	// read ("in case it is currently being cloned into"), which is exactly what a
	// bake killed halfway through a clone leaves behind. Nothing else uses this
	// name, so the directory goes: it holds a copy of the box's disk.
	if delErr != nil {
		_ = c.m.lima.StopForce(delCtx, c.Name)
	}
	if err := os.RemoveAll(dir); err != nil {
		if delErr != nil {
			return fmt.Errorf("delete the clone %s: %v; and its directory %s: %w", c.Name, delErr, dir, err)
		}
		return fmt.Errorf("delete the clone's directory %s: %w", dir, err)
	}
	return nil
}

// DiskUsed is what a file occupies on the host: the blocks it really holds
// (st_blocks), which is what a copy of it needs, and not the size it may grow
// to.
func DiskUsed(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		return int64(st.Blocks) * 512, true
	}
	return info.Size(), true
}
