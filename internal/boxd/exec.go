package boxd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExecSpec describes the process boxd must start for one session.
type ExecSpec struct {
	SessionID    string
	Model        string // "<provider>/<model>" as the caller wrote it
	Cwd          string
	SessionDir   string // per-session directory the executor may write to
	ResumeRootID string // executor transcript id to resume, "" for a fresh session
}

// Executor is the adapter for one agent product. Every adapter is driven over
// ACP; they differ in how the process is started and whether it keeps a
// resumable transcript.
type Executor interface {
	// Name is the executor name used in the contract ("prime", "opencode", "claude").
	Name() string
	// Probe reports availability and version for "boxctl info".
	Probe(ctx context.Context) ExecutorInfo
	// Argv returns the command line and extra environment for one session.
	Argv(spec ExecSpec) ([]string, []string, error)
	// TranscriptIDs lists the resumable transcripts in a session directory.
	// An adapter without a transcript returns nil and cannot resume.
	TranscriptIDs(dir string) []string
	// Configured reports whether the executor's credentials and configuration
	// are in the secrets directory the box got from Nomad.
	Configured() bool
}

// splitModel splits "<provider>/<model>" at the first slash.
func splitModel(s string) (provider, model string) {
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// probeEnv is the environment a version probe runs in: the executor's own home
// below the runtime directory, exactly the HOME its sessions get.
//
// A probe is not a read-only question. `opencode --version` creates its data
// directory and a log in $HOME, so a probe that inherits boxd's own HOME writes
// onto the box disk (boxd runs as root there) the moment somebody asks
// `boxctl info`. The home of an executor is on the tmpfs, and it exists whether
// or not the box has credentials for that executor.
func probeEnv(homeDir string) []string {
	env := os.Environ()
	if homeDir == "" {
		return env
	}
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return env
	}
	return appendFiltered(env, "HOME", "HOME="+homeDir)
}

// probeVersion runs "<bin> --version" with env and returns the first line.
func probeVersion(ctx context.Context, bin string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line, nil
}

// readEnvFile reads a KEY=VALUE file (blank lines and # comments ignored).
func readEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// buildExecutor returns the adapter for a session; it fails for an unknown name.
//
// Every directory an adapter is given that may hold a credential or the product's
// own settings is below the runtime directory, which is a tmpfs: the box's disk
// keeps sessions and transcripts, never a secret.
func buildExecutor(name string, cfg DaemonConfig) (Executor, error) {
	switch name {
	case "prime":
		return &primeExecutor{
			bin:       cfg.PrimeAgent,
			configDir: cfg.primeConfigDir(),
			envFile:   filepath.Join(cfg.SecretsDir, "prime", "env"),
			homeDir:   filepath.Join(cfg.executorsDir(), "prime-home"),
			secrets:   filepath.Join(cfg.SecretsDir, "prime"),
		}, nil
	case "opencode":
		return &opencodeExecutor{
			bin:     cfg.OpenCode,
			envFile: filepath.Join(cfg.SecretsDir, "opencode", "env"),
			homeDir: filepath.Join(cfg.executorsDir(), "opencode-home"),
			secrets: filepath.Join(cfg.SecretsDir, "opencode"),
		}, nil
	case "claude":
		return &claudeExecutor{
			bin:     cfg.ClaudeACP,
			envFile: filepath.Join(cfg.SecretsDir, "claude", "env"),
			homeDir: filepath.Join(cfg.executorsDir(), "claude-home"),
			secrets: filepath.Join(cfg.SecretsDir, "claude"),
		}, nil
	}
	return nil, Errorf(CodeInvalid, "unknown executor %q (want prime, opencode or claude)", name)
}

// executorEnv builds the environment of one executor process: inherited
// environment, the adapter's config directory and options, then the secret env
// file (which wins).
func executorEnv(envFile, homeDir string, extra map[string]string) ([]string, error) {
	env := os.Environ()
	if homeDir != "" && dirExists(homeDir) {
		env = appendFiltered(env, "HOME", "HOME="+homeDir)
	}
	for k, v := range extra {
		if v == "" {
			continue
		}
		env = appendFiltered(env, k, k+"="+v)
	}
	fromFile, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	for _, kv := range fromFile {
		k := kv[:strings.IndexByte(kv, '=')]
		env = appendFiltered(env, k, kv)
	}
	return env, nil
}

// appendFiltered sets key=value in env, replacing an existing assignment.
func appendFiltered(env []string, key, kv string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, kv)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---------------------------------------------------------------- prime -----

// primeExecutor runs "prime-agent --mode acp": one process per session, with
// explicit provider and model so no global settings file decides anything.
type primeExecutor struct {
	bin       string
	configDir string // PRIME_AGENT_CODING_AGENT_DIR ("" = leave the default)
	envFile   string
	homeDir   string
	secrets   string
}

func (p *primeExecutor) Name() string { return "prime" }

// Configured reports whether the box got anything in /run/box/secrets/prime:
// settings.json plus auth.json is what a custom provider needs, and without
// them a turn fails with the provider's own error.
func (p *primeExecutor) Configured() bool { return dirHasFiles(p.secrets) }

func (p *primeExecutor) Probe(ctx context.Context) ExecutorInfo {
	info := ExecutorInfo{Name: "prime"}
	path, err := exec.LookPath(p.bin)
	if err != nil {
		info.Reason = fmt.Sprintf("prime-agent not found (%q); the guest image installs it", p.bin)
		return info
	}
	v, err := probeVersion(ctx, path, probeEnv(p.homeDir), "--version")
	if err != nil {
		info.Reason = "prime-agent --version failed: " + err.Error()
		return info
	}
	info.Version = v
	info.Available = true
	configured := p.Configured()
	info.Configured = &configured
	if !configured {
		info.Reason = fmt.Sprintf("no credentials in %s: the box started without the executors variable, so runs `box executors set --from-dir DIR` and restarts the box", p.secrets)
	}
	return info
}

func (p *primeExecutor) Argv(spec ExecSpec) ([]string, []string, error) {
	argv := []string{p.bin, "--mode", "acp", "--session-dir", spec.SessionDir}
	if spec.Model != "" {
		provider, model := splitModel(spec.Model)
		if provider == "" {
			return nil, nil, Errorf(CodeInvalid, "model %q must look like <provider>/<model>", spec.Model)
		}
		argv = append(argv, "--provider", provider, "--model", model)
	}
	if spec.ResumeRootID != "" {
		argv = append(argv, "-r", spec.ResumeRootID)
	}
	env, err := executorEnv(p.envFile, p.homeDir, map[string]string{
		"PRIME_AGENT_CODING_AGENT_DIR": p.configDir,
	})
	if err != nil {
		return nil, nil, err
	}
	return argv, env, nil
}

func (p *primeExecutor) TranscriptIDs(dir string) []string { return jsonlIDs(dir) }

// jsonlIDs lists the transcript ids in a prime session directory: the file name
// without ".jsonl" is the root session id that "-r" expects.
func jsonlIDs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".jsonl"))
	}
	sort.Strings(ids)
	return ids
}

// ------------------------------------------------------------- opencode -----

// opencodeExecutor runs the opencode ACP adapter ("opencode acp"). It has no
// per-session transcript in a directory of our choosing, so sessions are not
// resumable across executor restarts.
type opencodeExecutor struct {
	bin     string
	envFile string
	homeDir string
	secrets string
}

func (o *opencodeExecutor) Name() string { return "opencode" }

// Configured reports whether the box got an opencode.json: opencode reads its
// providers from that file, and a custom provider cannot be selected without it.
func (o *opencodeExecutor) Configured() bool {
	return fileExists(filepath.Join(o.secrets, "opencode.json"))
}

func (o *opencodeExecutor) Probe(ctx context.Context) ExecutorInfo {
	info := ExecutorInfo{Name: "opencode"}
	path, err := exec.LookPath(o.bin)
	if err != nil {
		info.Reason = fmt.Sprintf("opencode not found (%q); the guest image installs it from npm", o.bin)
		return info
	}
	if v, err := probeVersion(ctx, path, probeEnv(o.homeDir), "--version"); err == nil {
		info.Version = v
	}
	info.Available = true
	configured := o.Configured()
	info.Configured = &configured
	if !configured {
		info.Reason = fmt.Sprintf("no opencode.json in %s: the box started without the executors variable, so runs `box executors set --from-dir DIR` and restarts the box", o.secrets)
	}
	return info
}

func (o *opencodeExecutor) Argv(spec ExecSpec) ([]string, []string, error) {
	env, err := executorEnv(o.envFile, o.homeDir, map[string]string{
		"BOX_MODEL": spec.Model,
	})
	if err != nil {
		return nil, nil, err
	}
	return []string{o.bin, "acp"}, env, nil
}

func (o *opencodeExecutor) TranscriptIDs(string) []string { return nil }

// --------------------------------------------------------------- claude -----

// claudeExecutor runs the Zed claude-code ACP adapter. Like opencode it keeps no
// transcript of ours, so it cannot resume a session.
type claudeExecutor struct {
	bin     string
	envFile string
	homeDir string
	secrets string
}

func (c *claudeExecutor) Name() string { return "claude" }

// Configured reports whether the box got anything in /run/box/secrets/claude.
func (c *claudeExecutor) Configured() bool { return dirHasFiles(c.secrets) }

func (c *claudeExecutor) Probe(ctx context.Context) ExecutorInfo {
	info := ExecutorInfo{Name: "claude"}
	path, err := exec.LookPath(c.bin)
	if err != nil {
		info.Reason = fmt.Sprintf("claude-code-acp not found (%q); install @zed-industries/claude-code-acp from the npm registry, or let boxd run the executors the image ships", c.bin)
		return info
	}
	if v, err := probeVersion(ctx, path, probeEnv(c.homeDir), "--version"); err == nil {
		info.Version = v
	}
	info.Available = true
	configured := c.Configured()
	info.Configured = &configured
	if !configured {
		info.Reason = fmt.Sprintf("no credentials in %s (box executors set --from-dir DIR)", c.secrets)
	}
	return info
}

func (c *claudeExecutor) Argv(spec ExecSpec) ([]string, []string, error) {
	env, err := executorEnv(c.envFile, c.homeDir, map[string]string{
		"BOX_MODEL": spec.Model,
	})
	if err != nil {
		return nil, nil, err
	}
	return []string{c.bin}, env, nil
}

func (c *claudeExecutor) TranscriptIDs(string) []string { return nil }

// --------------------------------------------------------------- config -----

// prepareExecutorDirs copies the read-only secret directories into the runtime
// directory (a tmpfs), so an agent can write its own settings without touching
// the driver's copy in /run/box/secrets and without depending on the guest's
// HOME. Nothing here may reach the box disk: an executor writes credentials into
// the directory it is given, and a box disk outlives the session.
func prepareExecutorDirs(secretsDir, runtimeDir string) error {
	root := filepath.Join(runtimeDir, "executors")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}
	for _, name := range []string{"prime", "opencode", "claude"} {
		// The HOME of every executor exists even without credentials: an executor
		// that finds no HOME of its own writes its configuration (and whatever it
		// derives from the credential) into the box user's home, which is on the
		// box disk.
		if err := os.MkdirAll(filepath.Join(root, name+"-home"), 0o700); err != nil {
			return fmt.Errorf("create the home of the %s executor: %w", name, err)
		}
		src := filepath.Join(secretsDir, name)
		if !dirExists(src) {
			continue
		}
		dst := filepath.Join(root, name)
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}
	return prepareOpenCodeConfig(secretsDir, runtimeDir)
}

// prepareOpenCodeConfig puts the box's opencode.json where opencode looks for
// it: $HOME/.config/opencode/opencode.json, with HOME set to the executor's own
// directory (on the tmpfs) for every session. The env file is not copied there: it holds the
// credential and boxd reads it as environment variables.
//
// The image's own /etc/opencode/opencode.json is the system configuration and
// outranks this file, so the box cannot be talked out of it; it only says what
// applies to everyone.
func prepareOpenCodeConfig(secretsDir, runtimeDir string) error {
	src := filepath.Join(secretsDir, "opencode")
	if !dirExists(src) {
		return nil
	}
	home := filepath.Join(runtimeDir, "executors", "opencode-home")
	dst := filepath.Join(home, ".config", "opencode")
	for _, dir := range []string{home, dst} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || e.Name() == "env" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func isEmptyDir(p string) bool {
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) == 0
}

// dirHasFiles reports whether p is a directory with at least one regular file
// in it (readable by the box user, which is who runs boxd's executors).
func dirHasFiles(p string) bool {
	ents, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

// fileExists reports whether p is a regular file.
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// copyTree copies a small directory tree (config files only) with owner-only
// permissions on everything it creates. A file the guest already has and that is
// not older than the source is left alone: the executor writes its own state into
// the same directory, and a box that gets new credentials keeps them.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		srcInfo, err := os.Stat(s)
		if err != nil {
			return err
		}
		if dstInfo, err := os.Stat(d); err == nil && !srcInfo.ModTime().After(dstInfo.ModTime()) {
			continue
		}
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// modelOption applies the requested model through the ACP config option when an
// adapter exposes one, which is how non-prime adapters pick a model without
// command line flags. Failures are not fatal: the adapter's own configuration
// stays in charge.
func modelOption(configOptions []configOption, model string) (configID, value string, ok bool) {
	provider, name := splitModel(model)
	for _, o := range configOptions {
		if o.ID != "model" && o.Category != "model" {
			continue
		}
		// prime and friends express the model as a JSON ["provider","model"] pair.
		var pair []string
		if json.Unmarshal([]byte(o.CurrentValue), &pair) == nil && len(pair) == 2 {
			b, _ := json.Marshal([]string{provider, name})
			return o.ID, string(b), true
		}
		if model != "" {
			return o.ID, model, true
		}
	}
	return "", "", false
}
