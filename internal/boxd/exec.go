package boxd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// Env returns the environment every session of this executor runs with,
	// without a model: its HOME, its XDG_CONFIG_HOME and its PATH. `boxctl exec`
	// starts a command in it, and Argv builds the same one for a session.
	Env() ([]string, error)
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

// probeEnv is the environment a version probe runs in: exactly the HOME and the
// XDG_CONFIG_HOME its sessions get, and the bundle's bin in front of PATH.
//
// A probe is not a read-only question. `opencode --version` creates its data
// directory and a log in $HOME, so a probe that inherits boxd's own HOME writes
// into root's home the moment somebody asks `boxctl info`.
//
// platformBin is the bundle's bin, in front of PATH: a probe must find the
// executors where the daemon would run them, so `boxctl info` reports the
// versions the box actually uses.
func probeEnv(dirs executorDirs, platformBin string) []string {
	env := os.Environ()
	env = appendFiltered(env, "PATH", "PATH="+withPathFront(pathOf(env), platformBin))
	if dirs.home == "" {
		return env
	}
	if err := os.MkdirAll(dirs.home, 0o700); err != nil {
		return env
	}
	env = appendFiltered(env, "HOME", "HOME="+dirs.home)
	if dirs.config != "" && os.MkdirAll(dirs.config, 0o700) == nil {
		env = appendFiltered(env, "XDG_CONFIG_HOME", "XDG_CONFIG_HOME="+dirs.config)
	}
	return env
}

// executorDirs are the two directories an executor process writes into besides
// its session directory.
//
// home is its HOME, on the box disk (below the state directory): an executor
// keeps caches, downloaded runtimes and virtual environments there and runs
// programs out of them, which the guest's /run cannot take -- it is a small tmpfs
// mounted noexec. config is its XDG_CONFIG_HOME, on the tmpfs: a configuration an
// executor writes for itself stays off the disk like the credentials it was
// given. The credentials never go to home: they are in the runtime copy of the
// secrets (and prime's PRIME_AGENT_CODING_AGENT_DIR points there) or in the
// environment.
type executorDirs struct {
	home   string
	config string
}

func (c DaemonConfig) dirsOf(name string) executorDirs {
	return executorDirs{
		home:   filepath.Join(c.StateDir, "homes", name),
		config: filepath.Join(c.executorsDir(), name+"-config"),
	}
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
// own settings is below the runtime directory, which is a tmpfs; its HOME, for
// caches and runtimes, is on the box disk (see executorDirs).
func buildExecutor(name string, cfg DaemonConfig) (Executor, error) {
	switch name {
	case "prime":
		return &primeExecutor{
			bin:         cfg.PrimeAgent,
			configDir:   cfg.primeConfigDir(),
			envFile:     filepath.Join(cfg.SecretsDir, "prime", "env"),
			dirs:        cfg.dirsOf("prime"),
			secrets:     filepath.Join(cfg.SecretsDir, "prime"),
			platformBin: cfg.platformBinDir(),
		}, nil
	case "opencode":
		return &opencodeExecutor{
			bin:         cfg.OpenCode,
			envFile:     filepath.Join(cfg.SecretsDir, "opencode", "env"),
			dirs:        cfg.dirsOf("opencode"),
			secrets:     filepath.Join(cfg.SecretsDir, "opencode"),
			platformBin: cfg.platformBinDir(),
		}, nil
	case "claude":
		return &claudeExecutor{
			bin:         cfg.ClaudeACP,
			envFile:     filepath.Join(cfg.SecretsDir, "claude", "env"),
			dirs:        cfg.dirsOf("claude"),
			secrets:     filepath.Join(cfg.SecretsDir, "claude"),
			platformBin: cfg.platformBinDir(),
		}, nil
	}
	return nil, Errorf(CodeInvalid, "unknown executor %q (want prime, opencode or claude)", name)
}

// executorEnv builds the environment of one executor process: inherited
// environment, the adapter's config directory and options, then the secret env
// file (which wins).
//
// PATH is the bundle's bin (platformBin) first, then the PATH boxd itself runs
// with, then the executor's own home directories. The bundle comes first so an
// executor the box carries is the one that runs, whatever the image installed.
func executorEnv(envFile string, dirs executorDirs, platformBin string, extra map[string]string) ([]string, error) {
	env := os.Environ()
	if dirs.home != "" && dirExists(dirs.home) {
		env = appendFiltered(env, "HOME", "HOME="+dirs.home)
	}
	path := withPathFront(pathOf(env), platformBin)
	if dirs.home != "" && dirExists(dirs.home) {
		// What an agent installs for itself lands in its HOME, and a tool it has
		// just installed is "command not found" without it on PATH. The bundle
		// comes first, so a tool it carries still runs whatever the box holds.
		path = withPath(path, homePathDirs(dirs.home)...)
	}
	if path != pathOf(env) {
		env = appendFiltered(env, "PATH", "PATH="+path)
	}
	if dirs.config != "" && dirExists(dirs.config) {
		env = appendFiltered(env, "XDG_CONFIG_HOME", "XDG_CONFIG_HOME="+dirs.config)
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

// homePathDirs are the directories an executor's own programs are installed in,
// in the order they go on PATH: what `go install`, `uv tool` and pip --user write
// to, then the ones bun, cargo and deno use. The last three are there only when
// they exist -- an image does not carry them, the agent's installer makes them --
// because that is what tells a tool the agent installed from one the box never
// had.
func homePathDirs(home string) []string {
	if home == "" {
		return nil
	}
	dirs := []string{filepath.Join(home, "go", "bin"), filepath.Join(home, ".local", "bin")}
	for _, rel := range []string{".bun/bin", ".cargo/bin", ".deno/bin"} {
		if dir := filepath.Join(home, rel); dirExists(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
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
	bin         string
	configDir   string // PRIME_AGENT_CODING_AGENT_DIR ("" = leave the default)
	envFile     string
	dirs        executorDirs
	secrets     string
	platformBin string // the platform bundle's bin, first on PATH
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
		info.Reason = fmt.Sprintf("prime-agent not found (%q); the executors a box runs are in the platform bundle's bin/", p.bin)
		return info
	}
	v, err := probeVersion(ctx, path, probeEnv(p.dirs, p.platformBin), "--version")
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

func (p *primeExecutor) Env() ([]string, error) {
	return executorEnv(p.envFile, p.dirs, p.platformBin, map[string]string{
		"PRIME_AGENT_CODING_AGENT_DIR": p.configDir,
	})
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
	env, err := p.Env()
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
	bin         string
	envFile     string
	dirs        executorDirs
	secrets     string
	platformBin string // the platform bundle's bin, first on PATH
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
		info.Reason = fmt.Sprintf("opencode not found (%q); the executors a box runs are in the platform bundle's bin/", o.bin)
		return info
	}
	if v, err := probeVersion(ctx, path, probeEnv(o.dirs, o.platformBin), "--version"); err == nil {
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

func (o *opencodeExecutor) Env() ([]string, error) {
	return executorEnv(o.envFile, o.dirs, o.platformBin, nil)
}

func (o *opencodeExecutor) Argv(spec ExecSpec) ([]string, []string, error) {
	env, err := o.Env()
	if err != nil {
		return nil, nil, err
	}
	if spec.Model != "" {
		env = appendFiltered(env, "BOX_MODEL", "BOX_MODEL="+spec.Model)
	}
	return []string{o.bin, "acp"}, env, nil
}

func (o *opencodeExecutor) TranscriptIDs(string) []string { return nil }

// --------------------------------------------------------------- claude -----

// claudeExecutor runs the Zed claude-code ACP adapter. Like opencode it keeps no
// transcript of ours, so it cannot resume a session.
type claudeExecutor struct {
	bin         string
	envFile     string
	dirs        executorDirs
	secrets     string
	platformBin string // the platform bundle's bin, first on PATH
}

func (c *claudeExecutor) Name() string { return "claude" }

// Configured reports whether the box got anything in /run/box/secrets/claude.
func (c *claudeExecutor) Configured() bool { return dirHasFiles(c.secrets) }

func (c *claudeExecutor) Probe(ctx context.Context) ExecutorInfo {
	info := ExecutorInfo{Name: "claude"}
	path, err := exec.LookPath(c.bin)
	if err != nil {
		info.Reason = fmt.Sprintf("claude-code-acp not found (%q); install @zed-industries/claude-code-acp from the npm registry and add it to the platform bundle the box mounts", c.bin)
		return info
	}
	if v, err := probeVersion(ctx, path, probeEnv(c.dirs, c.platformBin), "--version"); err == nil {
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

func (c *claudeExecutor) Env() ([]string, error) {
	return executorEnv(c.envFile, c.dirs, c.platformBin, nil)
}

func (c *claudeExecutor) Argv(spec ExecSpec) ([]string, []string, error) {
	env, err := c.Env()
	if err != nil {
		return nil, nil, err
	}
	if spec.Model != "" {
		env = appendFiltered(env, "BOX_MODEL", "BOX_MODEL="+spec.Model)
	}
	return []string{c.bin}, env, nil
}

func (c *claudeExecutor) TranscriptIDs(string) []string { return nil }

// --------------------------------------------------------------- config -----

// executorNames are the executors boxd knows, in the order it reports them.
var executorNames = []string{"prime", "opencode", "claude"}

// secretsDigest fingerprints the executor credentials the driver put on the
// tmpfs: every file of every executor's directory, by name and content.
//
// It is what a reload compares, so that a reload that changed nothing restarts
// nothing. The modification time is deliberately not part of it: Nomad
// re-renders a template that ends with the same bytes, and a rewrite is not a
// new credential.
func secretsDigest(cfg DaemonConfig) string {
	h := sha256.New()
	for _, name := range executorNames {
		root := filepath.Join(cfg.SecretsDir, name)
		var entries []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			sum := sha256.Sum256(b)
			entries = append(entries, name+"/"+rel+"\x00"+hex.EncodeToString(sum[:]))
			return nil
		})
		sort.Strings(entries)
		for _, e := range entries {
			_, _ = h.Write([]byte(e + "\n"))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// prepareExecutorDirs copies the read-only secret directories into the runtime
// directory (a tmpfs), so an agent can write its own settings without touching
// the driver's copy in /run/box/secrets, and creates every executor's HOME (on
// the disk) and XDG_CONFIG_HOME (on the tmpfs). No credential goes to the disk:
// an executor gets it from the runtime copy or from its environment.
func prepareExecutorDirs(cfg DaemonConfig) error {
	root := cfg.executorsDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", root, err)
	}
	for _, name := range executorNames {
		// The HOME of every executor exists even without credentials: an executor
		// that finds no HOME of its own writes into the home of the user boxd
		// runs as.
		dirs := cfg.dirsOf(name)
		for _, dir := range []string{dirs.home, dirs.config} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("create %s for the %s executor: %w", dir, name, err)
			}
		}
		src := filepath.Join(cfg.SecretsDir, name)
		if !dirExists(src) {
			continue
		}
		dst := filepath.Join(root, name)
		if err := copyTree(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
	}
	if err := prepareOpenCodeConfig(cfg); err != nil {
		return err
	}
	discoverModels(cfg)
	return nil
}

// prepareOpenCodeConfig puts the box's opencode.json where opencode looks for
// it: $XDG_CONFIG_HOME/opencode/opencode.json, with XDG_CONFIG_HOME set to the
// executor's own directory on the tmpfs for every session. The env file is not
// copied there: it holds the credential and boxd reads it as environment
// variables.
//
// The image's own /etc/opencode/opencode.json is the system configuration and
// outranks this file, so the box cannot be talked out of it; it only says what
// applies to everyone.
func prepareOpenCodeConfig(cfg DaemonConfig) error {
	src := filepath.Join(cfg.SecretsDir, "opencode")
	if !dirExists(src) {
		return nil
	}
	dst := filepath.Join(cfg.dirsOf("opencode").config, "opencode")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
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

// copyTree brings a small directory tree of credentials (config files only)
// into the executor's runtime directory, with owner-only permissions on
// everything it creates.
//
// What decides a copy is the content of the source, not its time: a rotated
// credential may well carry the same mtime as the one it replaces. The sha256 of
// every file copied last time is kept next to dst (dst + ".synced.json", on the
// same tmpfs), and a file is copied again only when its source content changed
// or the copy is gone. A file whose source did not change is left alone, because
// the executor writes its own state (a refreshed token, say) into the same
// directory. A file that was copied before and is gone from the source is
// removed; files the executor created itself are never touched.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	manifest := dst + ".synced.json"
	synced := map[string]string{}
	if b, err := os.ReadFile(manifest); err == nil {
		// A manifest that does not parse means nothing is known: copy everything.
		_ = json.Unmarshal(b, &synced)
	}
	current := map[string]string{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			// Nomad renders regular files; anything else is not a credential.
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		digest := hex.EncodeToString(sum[:])
		current[rel] = digest
		if synced[rel] == digest && fileExists(target) {
			return nil
		}
		return writeFileAtomic(target, b, 0o600)
	})
	if err != nil {
		return err
	}
	for rel := range synced {
		if _, ok := current[rel]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dst, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	b, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return writeFileAtomic(manifest, b, 0o600)
}

// writeFileAtomic replaces path with data in one rename, so an executor that
// reads the file while a reload runs sees the old content or the new one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, perm)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
	}
	return werr
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

// pathOf is PATH in env, "" when it is not set.
func pathOf(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "PATH="); ok {
			return v
		}
	}
	return ""
}

// defaultPath is the PATH a process gets when boxd's own environment has none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// withPathFront puts dirs in front of a PATH that does not have them yet, in the
// order given. It is how the platform bundle's bin comes before everything else:
// an empty dir is skipped.
func withPathFront(path string, dirs ...string) string {
	if path == "" {
		path = defaultPath
	}
	have := map[string]bool{}
	for _, d := range filepath.SplitList(path) {
		have[d] = true
	}
	var front []string
	for _, d := range dirs {
		if d == "" || have[d] {
			continue
		}
		front = append(front, d)
		have[d] = true
	}
	if len(front) == 0 {
		return path
	}
	return strings.Join(front, string(filepath.ListSeparator)) + string(filepath.ListSeparator) + path
}

// withPath appends dirs to a PATH that does not have them yet.
func withPath(path string, dirs ...string) string {
	if path == "" {
		path = defaultPath
	}
	have := map[string]bool{}
	for _, d := range filepath.SplitList(path) {
		have[d] = true
	}
	for _, d := range dirs {
		if !have[d] {
			path += string(filepath.ListSeparator) + d
			have[d] = true
		}
	}
	return path
}
