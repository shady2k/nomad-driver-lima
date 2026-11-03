package limactl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/identity"
)

// CommandError is returned when limactl exits non-zero. It keeps both streams so
// drivers can put the real reason in a task event instead of "exit status 1".
type CommandError struct {
	Args   []string
	Stdout string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Stdout)
	}
	if msg == "" {
		return fmt.Sprintf("limactl %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("limactl %s: %v: %s", strings.Join(e.Args, " "), e.Err, msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

// IsNotFound reports whether err is a limactl "instance not found" failure.
func IsNotFound(err error) bool {
	var cerr *CommandError
	if !errors.As(err, &cerr) {
		return false
	}
	return strings.Contains(strings.ToLower(cerr.Stderr+cerr.Stdout), "no instance found")
}

// Client runs limactl. Bin and Home must be set; Env and Timeout are optional.
type Client struct {
	Bin  string
	Home string
	// Env is the base environment for every call. When nil, os.Environ() plus
	// LIMA_HOME=Home is used. Tests set it explicitly.
	Env []string
	// Identity, when set, is the unprivileged user every limactl call runs as:
	// the child gets that user's uid, gid and supplementary groups, and Env is
	// the complete environment it sees. A Nomad client that runs as root needs
	// it, because Lima refuses to run as root.
	Identity *identity.Identity
	// Timeout bounds a single call made with Run (list, version, edit, shell,
	// copy); create, start and stop go through RunLong and are bounded by the
	// caller's context only.
	Timeout time.Duration
}

// New returns a client for the given limactl binary and Lima home directory.
func New(bin, home string) *Client {
	if bin == "" {
		bin = "limactl"
	}
	return &Client{Bin: bin, Home: home, Timeout: 2 * time.Minute}
}

// Environ returns the environment used for limactl calls.
func (c *Client) Environ() []string {
	base := c.Env
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "LIMA_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LIMA_HOME="+c.Home)
}

// Command builds (but does not start) a limactl command with the right
// environment and identity. Used for long-running streaming calls too, so the
// guest log stream and `nomad alloc exec` run as the same user as every other
// limactl call.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = c.Environ()
	if c.Identity != nil {
		c.Identity.Configure(cmd)
	}
	return cmd
}

// Run executes a short limactl call, bounded by Timeout.
func (c *Client) Run(ctx context.Context, args ...string) (string, string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	return c.RunLong(ctx, args...)
}

// RunLong executes a limactl call that is bounded by the caller's context
// only: create converts the image into the disk, start waits for the guest's
// first boot, stop waits for its shutdown. Timeout would cut those short - on a
// first boot under vz it cut `limactl start` at two minutes while the guest was
// still coming up.
func (c *Client) RunLong(ctx context.Context, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.Command(ctx, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(), &CommandError{
			Args:   args,
			Stdout: stdout.String(),
			Stderr: stderr.String(),
			Err:    err,
		}
	}
	return stdout.String(), stderr.String(), nil
}

// Version returns the limactl version string, e.g. "2.2.0".
func (c *Client) Version(ctx context.Context) (string, error) {
	out, _, err := c.Run(ctx, "--version")
	if err != nil {
		return "", err
	}
	return parseVersion(out), nil
}

// List returns every instance visible through this LIMA_HOME.
func (c *Client) List(ctx context.Context) ([]*Instance, error) {
	out, _, err := c.Run(ctx, "list", "--format", "json")
	if err != nil {
		// `limactl list` fails when the home directory does not exist yet.
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseList([]byte(out))
}

// Get returns the named instance, or (nil, nil) when it does not exist.
func (c *Client) Get(ctx context.Context, name string) (*Instance, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, inst := range list {
		if inst.Name == name {
			return inst, nil
		}
	}
	return nil, nil
}

// Exists reports whether the instance is known to lima.
func (c *Client) Exists(ctx context.Context, name string) (bool, error) {
	inst, err := c.Get(ctx, name)
	return inst != nil, err
}

// InstanceDir returns <LIMA_HOME>/<name>.
func (c *Client) InstanceDir(name string) string { return filepath.Join(c.Home, name) }

// DiskPath returns the instance's disk file (possibly a symlink).
func (c *Client) DiskPath(name string) string {
	return filepath.Join(c.InstanceDir(name), "disk")
}

// Create creates the instance described by yamlPath without starting it.
func (c *Client) Create(ctx context.Context, name, yamlPath string, extra ...string) error {
	args := []string{"create", "--name", name, "--tty=false"}
	args = append(args, extra...)
	args = append(args, yamlPath)
	_, _, err := c.RunLong(ctx, args...)
	return err
}

// Start boots the instance and waits until Lima reports it running.
func (c *Client) Start(ctx context.Context, name string, extra ...string) error {
	args := []string{"start", "--tty=false"}
	args = append(args, extra...)
	args = append(args, name)
	_, _, err := c.RunLong(ctx, args...)
	return err
}

// StartCmd returns the limactl start command without running it, so it can be
// wrapped (cgroup scope) or streamed.
func (c *Client) StartCmd(ctx context.Context, name string) *exec.Cmd {
	return c.Command(ctx, "start", "--tty=false", name)
}

// Stop asks the guest to shut down and returns when limactl exits.
func (c *Client) Stop(ctx context.Context, name string) error {
	_, _, err := c.RunLong(ctx, "stop", name)
	return err
}

// StopForce hard-kills the VM.
func (c *Client) StopForce(ctx context.Context, name string) error {
	_, _, err := c.Run(ctx, "stop", "-f", name)
	return err
}

// Delete removes the instance directory (and, with it, any stray disk file).
// It is only used by the volume plugin, never by the task driver.
func (c *Client) Delete(ctx context.Context, name string) error {
	_, _, err := c.Run(ctx, "delete", "-f", name)
	return err
}

// EditOptions are the offline resize knobs of `limactl edit`.
type EditOptions struct {
	CPUs      int
	MemoryGiB float64
	DiskGiB   float64
}

// Edit rewrites the instance configuration. Lima refuses this on a running
// instance, so callers must stop the box first.
func (c *Client) Edit(ctx context.Context, name string, opts EditOptions) error {
	args := []string{"edit", "--tty=false"}
	if opts.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(opts.CPUs))
	}
	if opts.MemoryGiB > 0 {
		args = append(args, "--memory", formatGiB(opts.MemoryGiB))
	}
	if opts.DiskGiB > 0 {
		args = append(args, "--disk", formatGiB(opts.DiskGiB))
	}
	args = append(args, name)
	_, _, err := c.Run(ctx, args...)
	return err
}

func formatGiB(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Shell runs a command inside the guest and returns its output.
func (c *Client) Shell(ctx context.Context, name string, cmd []string) (string, string, error) {
	args := append([]string{"shell", name, "--"}, cmd...)
	return c.Run(ctx, args...)
}

// ShellCmd returns a limactl shell command for streaming use.
func (c *Client) ShellCmd(ctx context.Context, name string, cmd []string) *exec.Cmd {
	args := append([]string{"shell", name, "--"}, cmd...)
	return c.Command(ctx, args...)
}

// Copy copies files between the host and the guest. Guest paths are prefixed
// with "<instance>:". Lima picks rsync or scp automatically.
func (c *Client) Copy(ctx context.Context, srcs []string, dst string) error {
	args := append([]string{"copy"}, srcs...)
	args = append(args, dst)
	_, _, err := c.Run(ctx, args...)
	return err
}

// Stream runs a limactl command and copies its stdout/stderr to w.
func (c *Client) Stream(ctx context.Context, w io.Writer, args ...string) error {
	cmd := c.Command(ctx, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}
