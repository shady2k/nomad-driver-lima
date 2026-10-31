// Package lima calls the unmodified upstream CLI; no local RPC service is added.
package lima

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed guest.py
var guestProgram string

type Runner interface {
	Run(context.Context, []byte, ...string) ([]byte, error)
}
type CommandRunner struct {
	Binary, Home, UserHome string
	Diagnostics            string
	Credential             *syscall.Credential
}

func (r CommandRunner) Run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, r.Binary, args...)
	if r.Credential != nil {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: r.Credential}
	}
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + r.UserHome, "LIMA_HOME=" + r.Home}
	c.Stdin = bytes.NewReader(input)
	var out, errout bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errout
	if e := c.Run(); e != nil {
		if r.Diagnostics != "" && len(args) > 0 && args[0] != "shell" {
			_ = os.MkdirAll(r.Diagnostics, 0700)
			text := errout.String()
			text = regexp.MustCompile(`https?://[^\s"<>]+`).ReplaceAllStringFunc(text, func(v string) string {
				u, e := url.Parse(v)
				if e != nil {
					return "[url]"
				}
				u.User = nil
				u.RawQuery = ""
				u.Fragment = ""
				return u.String()
			})
			text = regexp.MustCompile(`(?i)(token|password|secret|api_key)([=: ]+)[^\s,;]+`).ReplaceAllString(text, "${1}${2}[redacted]")
			_ = os.WriteFile(filepath.Join(r.Diagnostics, fmt.Sprintf("%s-%d.stderr", args[0], time.Now().UnixNano())), []byte(text), 0600)
		}
		return out.Bytes(), fmt.Errorf("limactl %s failed: %w (control diagnostics retained; no payload logged)", args[0], e)
	}
	return out.Bytes(), nil
}

type Info struct {
	Protected                       bool
	Name, Status, Dir, VMType, Arch string
	CPUs                            int
	Memory, Disk                    int64
	HostAgentPID, DriverPID         int
	Param                           map[string]string
}
type Identity struct {
	MachineID string `json:"machine_id"`
	BootID    string `json:"boot_id"`
}
type Result struct {
	Code, Signal int
	Error        string
}
type Snapshot struct {
	StdoutEOF, StderrEOF       bool
	Phase                      string
	Result                     *Result
	Stdout, Stderr             []byte
	StdoutOffset, StderrOffset int64
}
type Request struct {
	Name, Owner, Key, Template string
	CPUs, MemoryMiB, DiskGiB   int
}
type Client struct {
	Runner Runner
	Home   string
}

var validName = regexp.MustCompile(`^(nomad-[a-f0-9]{24}|[a-z0-9][a-z0-9-]{0,31}-[a-f0-9]{12})$`)

func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (c *Client) Version(ctx context.Context) (string, error) {
	b, e := c.Runner.Run(ctx, nil, "--version")
	return strings.TrimSpace(string(b)), e
}
func (c *Client) List(ctx context.Context) ([]Info, error) {
	b, e := c.Runner.Run(ctx, nil, "list", "--json")
	if e != nil {
		return nil, e
	}
	var rows []Info
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var v Info
		e = dec.Decode(&v)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("invalid Lima JSON")
		}
		rows = append(rows, v)
	}
	return rows, nil
}

var ErrNotFound = errors.New("Lima instance not found")

func (c *Client) Get(ctx context.Context, name string) (*Info, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid instance name")
	}
	rows, e := c.List(ctx)
	if e != nil {
		return nil, e
	}
	for _, v := range rows {
		if v.Name == name {
			return &v, nil
		}
	}
	return nil, ErrNotFound
}
func (c *Client) ConfigHash(name string) (string, error) {
	if !validName.MatchString(name) {
		return "", fmt.Errorf("invalid instance name")
	}
	p := filepath.Join(c.Home, name, "lima.yaml")
	s, e := os.Lstat(p)
	if e != nil {
		return "", e
	}
	if !s.Mode().IsRegular() {
		return "", fmt.Errorf("instance config is not regular")
	}
	b, e := os.ReadFile(p)
	return Hash(b), e
}
func (c *Client) ValidateTemplate(ctx context.Context, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("absolute template path required")
	}
	_, e := c.Runner.Run(ctx, nil, "validate", path)
	return e
}
func (c *Client) Create(ctx context.Context, r Request) error {
	if !validName.MatchString(r.Name) {
		return fmt.Errorf("invalid instance name")
	}
	vm := "qemu"
	if runtime.GOOS == "darwin" {
		vm = "vz"
	}
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	probeJSON, _ := json.Marshal([]map[string]string{{"script": "#!/bin/sh\n# Nomad ownership parameters only: {{.Param.nomad_driver_id}} {{.Param.nomad_task_key}}\nexit 0"}})
	marker := `.param.nomad_driver_id = "` + r.Owner + `" | .param.nomad_task_key = "` + r.Key + `"`
	_, e := c.Runner.Run(ctx, nil, "create", "--tty=false", "--name="+r.Name, "--vm-type="+vm, "--arch="+arch, "--cpus="+strconv.Itoa(r.CPUs), "--memory="+strconv.FormatFloat(float64(r.MemoryMiB)/1024, 'f', 6, 64), "--disk="+strconv.Itoa(r.DiskGiB), "--mount-none", "--plain", "--set=.ssh.forwardAgent = false | .ssh.loadDotSSHPubKeys = false | .propagateProxyEnv = false | .vmOpts = {}", "--set=.probes += "+string(probeJSON), "--set="+marker, r.Template)
	return e
}
func (c *Client) Start(ctx context.Context, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	_, e := c.Runner.Run(ctx, nil, "start", "--tty=false", name)
	return e
}
func (c *Client) Stop(ctx context.Context, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	_, e := c.Runner.Run(ctx, nil, "stop", "--tty=false", name)
	return e
}

// Delete names exactly one confirmed stopped instance, never --force.
func (c *Client) Delete(ctx context.Context, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	_, e := c.Runner.Run(ctx, nil, "delete", "--tty=false", name)
	return e
}
func (c *Client) Guest(ctx context.Context, name string, payload any) (Snapshot, Identity, error) {
	var snap Snapshot
	var ident Identity
	if !validName.MatchString(name) {
		return snap, ident, fmt.Errorf("invalid instance name")
	}
	b, e := json.Marshal(payload)
	if e != nil {
		return snap, ident, e
	}
	out, e := c.Runner.Run(ctx, b, "shell", "--workdir=/", name, "--", "/usr/bin/python3", "-c", guestProgram)
	if e != nil {
		return snap, ident, e
	}
	var raw struct {
		StdoutEOF      bool `json:"stdout_eof"`
		StderrEOF      bool `json:"stderr_eof"`
		Phase          string
		Result         *Result
		Stdout, Stderr string
		StdoutOffset   int64  `json:"stdout_offset"`
		StderrOffset   int64  `json:"stderr_offset"`
		MachineID      string `json:"machine_id"`
		BootID         string `json:"boot_id"`
	}
	if e = json.Unmarshal(out, &raw); e != nil {
		return snap, ident, fmt.Errorf("invalid guest status JSON")
	}
	snap.StdoutEOF = raw.StdoutEOF
	snap.StderrEOF = raw.StderrEOF
	snap.Phase = raw.Phase
	snap.Result = raw.Result
	snap.StdoutOffset = raw.StdoutOffset
	snap.StderrOffset = raw.StderrOffset
	snap.Stdout, e = decode(raw.Stdout)
	if e != nil {
		return snap, ident, e
	}
	snap.Stderr, e = decode(raw.Stderr)
	ident = Identity{raw.MachineID, raw.BootID}
	return snap, ident, e
}
func (c *Client) Exec(ctx context.Context, name string, cmd []string) ([]byte, error) {
	if !validName.MatchString(name) || len(cmd) == 0 {
		return nil, fmt.Errorf("invalid guest command")
	}
	args := append([]string{"shell", "--workdir=/", name, "--"}, cmd...)
	return c.Runner.Run(ctx, nil, args...)
}
func (c *Client) CopyOut(ctx context.Context, name, guestPath, hostPath string) error {
	if !validName.MatchString(name) || !strings.HasPrefix(guestPath, "/") {
		return fmt.Errorf("invalid copy source")
	}
	_, e := c.Runner.Run(ctx, nil, "copy", name+":"+guestPath, hostPath)
	return e
}

// Stream uses the official SSH-based shell command. PTY resize is not emulated.
func (r CommandRunner) Stream(ctx context.Context, in io.Reader, out, errout io.Writer, args ...string) error {
	c := exec.CommandContext(ctx, r.Binary, args...)
	if r.Credential != nil {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: r.Credential}
	}
	c.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + r.UserHome, "LIMA_HOME=" + r.Home}
	c.Stdin = in
	c.Stdout = out
	c.Stderr = errout
	return c.Run()
}
func (c *Client) Stream(ctx context.Context, name string, command []string, in io.Reader, out, errout io.Writer) error {
	if !validName.MatchString(name) || len(command) == 0 {
		return fmt.Errorf("invalid guest exec")
	}
	r, ok := c.Runner.(interface {
		Stream(context.Context, io.Reader, io.Writer, io.Writer, ...string) error
	})
	if !ok {
		return fmt.Errorf("streaming runner unavailable")
	}
	return r.Stream(ctx, in, out, errout, append([]string{"shell", "--workdir=/", name, "--"}, command...)...)
}
