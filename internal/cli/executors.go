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

// executorTemplateSuffix marks an item of the executors Variable as a template
// source instead of a file. "prime/auth.json.tmpl" holds Nomad template text --
// `{{ with secret "kv/data/box/llm" }}...{{ end }}` -- and is rendered to
// "secrets/prime/auth.json", so the credential is read from Vault by Nomad at
// render time and never travels through the Variable, the job spec or any host.
const executorTemplateSuffix = ".tmpl"

// isTemplateKey reports whether an item key is a template source.
func isTemplateKey(key string) bool { return strings.HasSuffix(key, executorTemplateSuffix) }

// templateDestKey is the item key a template item renders to: the key without
// the suffix, so "prime/auth.json.tmpl" and a plain "prime/auth.json" are two
// ways to fill the same file.
func templateDestKey(key string) string { return strings.TrimSuffix(key, executorTemplateSuffix) }

// ExecutorFile describes one file of the executors variable, without its
// content: the file names are not secret, the values are.
type ExecutorFile struct {
	// Key is the item key: "prime/auth.json", or "prime/auth.json.tmpl" for a
	// template item.
	Key string `json:"key"`
	// Executor is the first path segment of the key.
	Executor string `json:"executor"`
	// Path is the path below the executor's directory: "auth.json", or
	// "auth.json.tmpl" for a template item, which renders to "auth.json".
	Path string `json:"path"`
	// Template is true when the item is template text that reads a secret at
	// render time rather than the content of a file.
	Template bool `json:"template,omitempty"`
	// Size is the content length in bytes.
	Size int `json:"size"`
}

// ExecutorItem is one item of the executors Variable as a box job renders it.
type ExecutorItem struct {
	// Key is the item key: "prime/auth.json" or "prime/auth.json.tmpl".
	Key string
	// Text is the item's own text when it is a template, and empty for a plain
	// item. A template holds the path it reads and no value: the job carries the
	// text, Nomad renders it, and the secret never leaves Vault until then.
	Text string
}

// template reports whether the item is a template source.
func (i ExecutorItem) template() bool { return isTemplateKey(i.Key) }

// destKey is the item key the item renders to: the same key for a plain item,
// the key without the suffix for a template ("prime/auth.json").
func (i ExecutorItem) destKey() string {
	if i.template() {
		return templateDestKey(i.Key)
	}
	return i.Key
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
		out = append(out, ExecutorFile{Key: key, Executor: executor, Path: file,
			Template: isTemplateKey(key), Size: len(value)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// executorItemsOf returns the items of a variable that a box job renders,
// sorted by key. A template item carries its own text, because the job embeds
// it: reading the text here is what keeps the secret out of the job, since a
// template item holds a Vault path where a plain item holds the value.
func executorItemsOf(items map[string]string) []ExecutorItem {
	out := make([]ExecutorItem, 0, len(items))
	for _, f := range executorFilesOf(items) {
		item := ExecutorItem{Key: f.Key}
		if f.Template {
			if _, _, ok := splitExecutorKey(templateDestKey(f.Key)); !ok {
				continue
			}
			item.Text = items[f.Key]
		}
		out = append(out, item)
	}
	return out
}

// ReadExecutorDir reads one executor's directory (for example a copy of
// ~/.prime/agent) into Variable items keyed <executor>/<relative path>. A file
// named "<file>.tmpl" becomes a template item, so a staging directory can hold
// the template of a credential instead of the credential.
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

// executorTemplates turns the items of the executors Variable into one task
// template per file.
//
// A plain item's template reads the item out of the Variable at render time, so
// a job spec (which is readable by anyone with read-job) never contains a
// credential. A template item is embedded as it is: Nomad renders its own text,
// which reads the credential from Vault with the task's workload identity, so
// neither the Variable nor the job holds a value.
func executorTemplates(items []ExecutorItem) []*api.Template {
	sorted := append([]ExecutorItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	templates := make([]*api.Template, 0, len(sorted))
	for _, item := range sorted {
		if _, _, ok := splitExecutorKey(item.destKey()); !ok {
			continue
		}
		dest := boxSecretsDirInTask + "/" + item.destKey()
		mode := "noop"
		perms := "0600"
		text := item.Text
		if !item.template() {
			// nomadVar returns the variable's items as a map of key to
			// NomadVarItem, so one item is `index . "key"` and its text is
			// `.Value`; the keys hold slashes and dots, which field syntax cannot
			// express. nomadVarExists guards the read: without it the renderer
			// blocks until the variable appears.
			text = fmt.Sprintf(
				`{{- if nomadVarExists %q -}}{{- with nomadVar %q -}}{{- (index . %q).Value -}}{{- end -}}{{- end -}}`,
				boxExecutorsVariable, boxExecutorsVariable, item.Key)
		}
		templates = append(templates, &api.Template{
			DestPath: &dest,
			// A missing Variable writes an empty file instead of failing the
			// task: a box without credentials still starts, and boxd says the
			// executor is not configured. A template item's Vault read is
			// Nomad's own business: while it fails, the task is not started.
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

// templateItemKeys names the template items of a staged set, so `box executors
// set` reports them as templates: an operator has to see that the variable holds
// a path, not a value.
func templateItemKeys(files map[string][]byte) []ExecutorKey {
	keys := make([]string, 0, len(files))
	for key := range files {
		if isTemplateKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([]ExecutorKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, ExecutorKey{File: key, Kind: "template"})
	}
	return out
}

// hasExecutorTemplate reports whether any item reads a secret at render time.
// Such a task needs a Vault block; a box made only of plain items does not, so
// its job stays usable on a cluster without Vault.
func hasExecutorTemplate(items []ExecutorItem) bool {
	for _, item := range items {
		if item.template() {
			return true
		}
	}
	return false
}

// validateExecutorTemplates checks the template items of a staged set.
//
// A template item must hold template text, and no two items may render to the
// same file: the renderer would write both to one path and which one wins would
// depend on the order Nomad happens to render them in.
func validateExecutorTemplates(files map[string][]byte) error {
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !isTemplateKey(key) {
			continue
		}
		if !strings.Contains(string(files[key]), "{{") {
			return usagef("%s ends in %s but holds no template text: such an item is a Nomad template, "+
				`for example {{ with secret "kv/data/box/llm" }}{{ .Data.data.key | toJSON }}{{ end }}, `+
				"and it must never hold the secret itself (name it without %s to store a file)",
				key, executorTemplateSuffix, executorTemplateSuffix)
		}
		if _, _, ok := splitExecutorKey(templateDestKey(key)); !ok {
			return usagef("%s renders to %q, which is not <executor>/<file>", key, templateDestKey(key))
		}
	}
	dest := make(map[string]string, len(keys))
	for _, key := range keys {
		if _, _, ok := splitExecutorKey(key); !ok {
			continue
		}
		rendered := key
		if isTemplateKey(key) {
			rendered = templateDestKey(key)
		}
		if first, ok := dest[rendered]; ok {
			return usagef("%s and %s both render to %s: keep one of them, "+
				"because the renderer would write both to the same file", first, key, rendered)
		}
		dest[rendered] = key
	}
	return nil
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
	// Kind is "credential" (a stored credential, keyed by provider),
	// "provider" (a provider definition) or "template" (a template item, which
	// holds a Vault path instead of a value).
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
// executors show` says which provider a box is configured for. Template items
// are reported as templates: they are not JSON, and what they hold is a path.
func storedKeys(items map[string]string) []ExecutorKey {
	var report []ExecutorKey
	for _, f := range executorFilesOf(items) {
		if f.Template {
			report = append(report, ExecutorKey{File: f.Key, Kind: "template"})
		}
	}
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

// executorKeysFromJob reads the executor file names back out of a registered box
// job, which is how `box status` shows what a box was given at creation. The name
// is the destination, so a template item appears as the file it rendered to
// ("prime/auth.json"), whatever the item in the variable is called.
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

// executorItems returns the items a new box job should render. A cluster
// without the variable is not an error: the box starts and the guest reports
// every executor as not configured.
func (a *App) executorItems(ctx context.Context) []ExecutorItem {
	v, err := a.executorsVariable(ctx)
	if err != nil {
		a.note("note   %s; the box starts without executor credentials", err)
		return nil
	}
	if v == nil {
		return nil
	}
	return executorItemsOf(v.Items)
}
