package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/nomad/api"
)

// boxExecutorsVariable is the Nomad Variable that carries the executor
// configuration and credentials of every box.
//
// A task may read the variables under nomad/jobs/<job> without an extra ACL
// policy, so one variable at nomad/jobs/box-executors is readable by every box
// in the namespace. Its items are the files of the guests'
// /run/box/secrets/<executor>/ directory, keyed by their path below it
// ("prime/auth.json", "opencode/opencode.json", ...).
//
// The box job never carries the values: it carries one template per item that
// reads the item out of the Variable, so the job spec holds file names only.
const boxExecutorsVariable = "nomad/jobs/box-executors"

// boxExecutorsVariableLimit is what Nomad accepts for one variable in total.
// A directory that is too large is refused with the files that make it large,
// not with the server's own RPC error.
const boxExecutorsVariableLimit = 64 << 10

// boxExecutorNames are the executors boxd can run, in the order the commands
// print them.
var boxExecutorNames = []string{"prime", "opencode", "claude"}

// boxExecutorRequired lists the files boxd cannot start the executor without:
// `box executors show` reports them, and a box's guest reports an executor
// without them as not configured.
var boxExecutorRequired = map[string][]string{
	"prime":    {"settings.json", "auth.json"},
	"opencode": {"opencode.json"},
	"claude":   {},
}

// boxSecretsDirInTask is where the executor files live in the task directory;
// the driver copies the task's secrets directory to /run/box/secrets in the
// guest, which is where boxd reads it.
const boxSecretsDirInTask = "secrets"

// ExecutorFile describes one file of the executors variable, without its
// content: the file names are not secret, the values are.
type ExecutorFile struct {
	// Key is the item key: "prime/auth.json".
	Key string `json:"key"`
	// Executor is the first path segment of the key.
	Executor string `json:"executor"`
	// Path is the path below the executor's directory: "auth.json".
	Path string `json:"path"`
	// Size is the content length in bytes.
	Size int `json:"size"`
}

// isBoxExecutor reports whether name is an executor boxd knows.
func isBoxExecutor(name string) bool {
	for _, known := range boxExecutorNames {
		if name == known {
			return true
		}
	}
	return false
}

// splitExecutorKey splits an item key into its executor and the path below it.
func splitExecutorKey(key string) (executor, file string, ok bool) {
	executor, file, ok = strings.Cut(key, "/")
	if !ok || file == "" || !isBoxExecutor(executor) {
		return "", "", false
	}
	if strings.Contains(file, "..") || strings.HasPrefix(file, "/") {
		return "", "", false
	}
	return executor, file, true
}

// executorFilesOf describes the items of a variable, sorted by key.
func executorFilesOf(items map[string]string) []ExecutorFile {
	out := make([]ExecutorFile, 0, len(items))
	for key, value := range items {
		executor, file, ok := splitExecutorKey(key)
		if !ok {
			continue
		}
		out = append(out, ExecutorFile{Key: key, Executor: executor, Path: file, Size: len(value)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// executorKeysOf returns the sorted item keys of a variable. These are what a
// box job template refers to.
func executorKeysOf(items map[string]string) []string {
	files := executorFilesOf(items)
	keys := make([]string, 0, len(files))
	for _, f := range files {
		keys = append(keys, f.Key)
	}
	return keys
}

// ReadExecutorDir reads one executor's directory (for example a copy of
// ~/.prime/agent) into Variable items keyed <executor>/<relative path>.
func ReadExecutorDir(executor, dir string) (map[string][]byte, error) {
	if !isBoxExecutor(executor) {
		return nil, usagef("%q is not an executor boxd knows: use one of %s", executor, strings.Join(boxExecutorNames, ", "))
	}
	files, err := readFileTree(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(files))
	for rel, data := range files {
		out[executor+"/"+rel] = data
	}
	return out, nil
}

// ReadExecutorsDir reads <dir>/<executor>/<file> into Variable items. The
// subdirectories are the executors; anything else is reported as a usage
// error, so a typo cannot silently produce a box without credentials.
func ReadExecutorsDir(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, wrapf(err, "cannot read %s", dir)
	}
	out := map[string][]byte{}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !isBoxExecutor(entry.Name()) {
			return nil, usagef("%s holds %q, which is not an executor boxd knows: use the subdirectories %s",
				dir, entry.Name(), strings.Join(boxExecutorNames, ", "))
		}
		files, err := ReadExecutorDir(entry.Name(), filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for key, data := range files {
			out[key] = data
		}
		found++
	}
	if found == 0 {
		return nil, usagef("%s has no executor directory: put the files under %s", dir, filepath.Join(dir, "prime"))
	}
	return out, nil
}

// readFileTree reads every regular file below root, keyed by its relative path
// with forward slashes. Checkouts and installed packages are skipped: they are
// machine-sized, belong to the package manager, and are not credentials.
func readFileTree(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) {
			return usagef("%s is not text: the executors variable holds text files only", path)
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, usagef("%s holds no files", root)
	}
	return out, nil
}

// executorTemplates turns item keys into one task template per file. The
// template reads the item out of the Variable at render time, so a job spec
// (which is readable by anyone with read-job) never contains a credential.
func executorTemplates(keys []string) []*api.Template {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	templates := make([]*api.Template, 0, len(sorted))
	for _, key := range sorted {
		if _, _, ok := splitExecutorKey(key); !ok {
			continue
		}
		dest := boxSecretsDirInTask + "/" + key
		mode := "noop"
		perms := "0600"
		// nomadVar returns the variable's items as a map of key to
		// NomadVarItem, so one item is `index . "key"` and its text is
		// `.Value`; the keys hold slashes and dots, which field syntax cannot
		// express. nomadVarExists guards the read: without it the renderer
		// blocks until the variable appears.
		text := fmt.Sprintf(
			`{{- if nomadVarExists %q -}}{{- with nomadVar %q -}}{{- (index . %q).Value -}}{{- end -}}{{- end -}}`,
			boxExecutorsVariable, boxExecutorsVariable, key)
		templates = append(templates, &api.Template{
			DestPath: &dest,
			// A missing Variable writes an empty file instead of failing the
			// task: a box without credentials still starts, and boxd says the
			// executor is not configured.
			EmbeddedTmpl: &text,
			// Reading a box must not restart its VM: the files are in the guest
			// after the driver's own file copy, and the box is restarted only
			// when the operator asks.
			ChangeMode: &mode,
			Perms:      &perms,
		})
	}
	return templates
}

// providerScopedFiles are the files that carry provider credentials or a
// provider definition. They are reduced to the chosen provider before anything
// is stored: a box runs one provider, so the others' credentials have no reason
// to travel in the cluster's variable.
var providerScopedFiles = []string{"prime/auth.json", "prime/models.json", "opencode/opencode.json"}

// ExecutorKey names the JSON keys one stored file holds, without any value.
// `box executors set` and `box executors show` print it, so an operator sees
// which credential is stored without ever seeing a token.
type ExecutorKey struct {
	// File is the item key: "prime/auth.json".
	File string `json:"file"`
	// Kind is "credential" (a stored credential, keyed by provider) or
	// "provider" (a provider definition).
	Kind string `json:"kind"`
	// Name is the provider the key belongs to.
	Name string `json:"name"`
	// Fields are the fields of a credential, for example access, refresh and
	// expires. Never their values.
	Fields []string `json:"fields,omitempty"`
}

// providerScoped finds the provider-scoped files of a staged set.
func providerScoped(files map[string][]byte) []string {
	var found []string
	for _, key := range providerScopedFiles {
		if _, ok := files[key]; ok {
			found = append(found, key)
		}
	}
	return found
}

// requireProvider refuses a set of files whose provider is not named. Every
// provider-scoped file holds credentials or definitions of (possibly many)
// providers, so which one to keep has to be said out loud.
func requireProvider(provider string, files map[string][]byte) error {
	if strings.TrimSpace(provider) != "" {
		return nil
	}
	scoped := providerScoped(files)
	if len(scoped) == 0 {
		return nil
	}
	return usagef("box executors set needs --provider NAME: %s hold credentials or provider definitions, "+
		"and only the chosen provider is stored (never the whole file)", strings.Join(scoped, ", "))
}

// selectProvider reduces the provider-scoped files to provider. Every other
// file is kept as it is. It reports the keys that are stored, and the provider
// keys that were dropped.
func selectProvider(files map[string][]byte, provider string) (map[string][]byte, []ExecutorKey, []string, error) {
	out := make(map[string][]byte, len(files))
	for key, data := range files {
		out[key] = data
	}
	if strings.TrimSpace(provider) == "" {
		return out, nil, nil, nil
	}
	var (
		report  []ExecutorKey
		dropped []string
	)
	for _, key := range providerScopedFiles {
		data, ok := files[key]
		if !ok {
			continue
		}
		doc, err := parseJSONObject(data)
		if err != nil {
			return nil, nil, nil, usagef("%s is not a JSON object: %v", key, err)
		}
		switch key {
		case "prime/auth.json":
			// One credential, not the whole store: `auth.json` holds one entry
			// per provider the host ever logged in to.
			credential, ok := doc[provider]
			if !ok {
				return nil, nil, nil, usagef("%s has no credential for %q: it holds %s. Run prime-agent and /login %s, "+
					"or name one of those with --provider", key, provider, orNone(jsonKeys(doc)), provider)
			}
			fields := jsonKeysOf(credential)
			kept := map[string]any{provider: credential}
			encoded, err := encodeJSON(kept)
			if err != nil {
				return nil, nil, nil, usagef("%s could not be reduced to %q: %v", key, provider, err)
			}
			out[key] = encoded
			report = append(report, ExecutorKey{File: key, Kind: "credential", Name: provider, Fields: fields})
			for _, other := range jsonKeys(doc) {
				if other != provider {
					dropped = append(dropped, key+": "+other)
				}
			}
		default:
			// A provider definition: `models.json` and opencode's
			// `opencode.json` both key their definitions by provider.
			providers, ok := doc["providers"].(map[string]any)
			if !ok || len(providers) == 0 {
				return nil, nil, nil, usagef("%s has no \"providers\" object", key)
			}
			definition, ok := providers[provider]
			if !ok {
				return nil, nil, nil, usagef("%s does not define the provider %q: it defines %s",
					key, provider, orNone(jsonKeys(providers)))
			}
			doc["providers"] = map[string]any{provider: definition}
			encoded, err := encodeJSON(doc)
			if err != nil {
				return nil, nil, nil, usagef("%s could not be reduced to %q: %v", key, provider, err)
			}
			out[key] = encoded
			report = append(report, ExecutorKey{File: key, Kind: "provider", Name: provider})
			for _, other := range jsonKeys(providers) {
				if other != provider {
					dropped = append(dropped, key+": "+other)
				}
			}
		}
	}
	return out, report, dropped, nil
}

// storedKeys reports the provider keys of the files that are stored, so `box
// executors show` says which provider a box is configured for.
func storedKeys(items map[string]string) []ExecutorKey {
	var report []ExecutorKey
	for _, key := range providerScopedFiles {
		text, ok := items[key]
		if !ok {
			continue
		}
		doc, err := parseJSONObject([]byte(text))
		if err != nil {
			report = append(report, ExecutorKey{File: key, Kind: "unreadable"})
			continue
		}
		switch key {
		case "prime/auth.json":
			for _, provider := range jsonKeys(doc) {
				report = append(report, ExecutorKey{File: key, Kind: "credential", Name: provider,
					Fields: jsonKeysOf(doc[provider])})
			}
		default:
			providers, _ := doc["providers"].(map[string]any)
			for _, provider := range jsonKeys(providers) {
				report = append(report, ExecutorKey{File: key, Kind: "provider", Name: provider})
			}
		}
	}
	return report
}

// parseJSONObject decodes one staged file as a JSON object.
func parseJSONObject(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// encodeJSON writes a JSON document the way the staged files are written.
func encodeJSON(doc map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// jsonKeys lists the keys of a JSON object, sorted.
func jsonKeys(doc map[string]any) []string {
	out := make([]string, 0, len(doc))
	for key := range doc {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// jsonKeysOf lists the keys of one JSON value, when it is an object.
func jsonKeysOf(value any) []string {
	doc, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return jsonKeys(doc)
}

// orNone renders a key list for a message.
func orNone(keys []string) string {
	if len(keys) == 0 {
		return "no key at all"
	}
	return strings.Join(keys, ", ")
}

// executorKeysFromJob reads the executor item keys back out of a registered
// box job, which is how `box status` shows what a box was given at creation.
func executorKeysFromJob(job *api.Job) []string {
	task, err := boxTask(job)
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(task.Templates))
	for _, t := range task.Templates {
		if t == nil {
			continue
		}
		key := strings.TrimPrefix(derefStr(t.DestPath, ""), boxSecretsDirInTask+"/")
		if _, _, ok := splitExecutorKey(key); ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// executorExecutorsOf lists the executors that have at least one file.
func executorsOf(files []ExecutorFile) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if !seen[f.Executor] {
			seen[f.Executor] = true
			out = append(out, f.Executor)
		}
	}
	sort.Strings(out)
	return out
}

// executorsVariable reads the executors variable; a missing variable is not an
// error, it means no executor is configured.
func (a *App) executorsVariable(ctx context.Context) (*api.Variable, error) {
	v, err := a.Nomad.VariableRead(ctx, boxExecutorsVariable)
	if err != nil {
		if isNotFoundErr(err) {
			return nil, nil
		}
		return nil, wrapf(err, "cannot read the variable %s", boxExecutorsVariable)
	}
	if v == nil {
		return nil, nil
	}
	return v, nil
}

// executorItemKeys returns the item keys a new box job should reference. A
// cluster without the variable is not an error: the box starts and the guest
// reports every executor as not configured.
func (a *App) executorItemKeys(ctx context.Context) []string {
	v, err := a.executorsVariable(ctx)
	if err != nil {
		a.note("note   %s; the box starts without executor credentials", err)
		return nil
	}
	if v == nil {
		return nil
	}
	return executorKeysOf(v.Items)
}
