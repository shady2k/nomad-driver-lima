package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// executorsSetOutput is the JSON document of `box executors set`.
type executorsSetOutput struct {
	Variable  string         `json:"variable"`
	Files     []ExecutorFile `json:"files"`
	Executors []string       `json:"executors"`
	Bytes     int            `json:"bytes"`
	Provider  string         `json:"provider,omitempty"`
	// Keys are the JSON keys the stored files hold, without any value.
	Keys []ExecutorKey `json:"keys,omitempty"`
	// Dropped lists the provider keys that were not stored.
	Dropped []string `json:"dropped,omitempty"`
}

// executorsShowOutput is the JSON document of `box executors show`.
type executorsShowOutput struct {
	Variable  string         `json:"variable"`
	Exists    bool           `json:"exists"`
	Files     []ExecutorFile `json:"files"`
	Executors []string       `json:"executors"`
	Missing   []string       `json:"missing,omitempty"`
	Bytes     int            `json:"bytes"`
	Keys      []ExecutorKey  `json:"keys,omitempty"`
	Warn      string         `json:"warning,omitempty"`
}

// cmdExecutors writes and reads the executor credentials the boxes start with.
func (a *App) cmdExecutors(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("box executors needs a subcommand: set, show or delete")
	}
	switch args[0] {
	case "set":
		return a.cmdExecutorsSet(ctx, args[1:])
	case "show", "ls":
		return a.cmdExecutorsShow(ctx, args[1:])
	case "delete", "rm":
		return a.cmdExecutorsDelete(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintf(a.ErrOut, "usage: box executors set --from-dir DIR --provider NAME [--executor NAME]\n"+
			"       box executors show [--json]\n"+
			"       box executors delete [--yes]\n\n"+
			"The files live in the Nomad variable %s and are rendered into every new\n"+
			"box at /run/box/secrets/<executor>/. DIR holds one subdirectory per executor\n"+
			"(prime/, opencode/), or is one executor's own directory with --executor.\n\n"+
			"--provider keeps one provider only: prime/auth.json is reduced to that provider's\n"+
			"credential, and prime/models.json and opencode/opencode.json to its definition.\n"+
			"The command prints the keys it stores, never a value.\n\n"+
			"An item whose name ends in .tmpl is a template instead of a file: its text is\n"+
			"Nomad template text that reads the secret itself, for example\n"+
			`  {"myprovider": {"type": "api_key", "key": {{ with secret "kv/data/box/llm" }}{{ .Data.data.key | toJSON }}{{ end }}}}`+"\n"+
			"and it is rendered to the same name without .tmpl. Such an item must never hold a\n"+
			"secret, only the path it is read from: the box job carries the template and a Vault\n"+
			"block, and Nomad reads the value at render time.\n", boxExecutorsVariable)
		return nil
	default:
		return usagef("unknown subcommand %q: use set, show or delete", args[0])
	}
}

// cmdExecutorsSet writes the executor files into the Nomad variable.
func (a *App) cmdExecutorsSet(ctx context.Context, args []string) error {
	fs := newFlagSet("executors set")
	fromDir := fs.String("from-dir", "", "directory with the executor files (required)")
	executor := fs.String("executor", "", "read DIR as this executor's own directory")
	provider := fs.String("provider", "", "the one provider to keep in auth.json, models.json and opencode.json (required)")
	dryRun := fs.Bool("dry-run", false, "show what would be written, change nothing")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	if strings.TrimSpace(*fromDir) == "" {
		return usagef("box executors set needs --from-dir DIR (one subdirectory per executor: prime/, opencode/)")
	}
	var (
		files map[string][]byte
		err   error
	)
	if *executor != "" {
		files, err = ReadExecutorDir(*executor, *fromDir)
	} else {
		files, err = ReadExecutorsDir(*fromDir)
	}
	if err != nil {
		return err
	}
	// Only the chosen provider is stored: a box runs one provider, and the
	// credentials of the others have no reason to sit in the cluster.
	if err := requireProvider(*provider, files); err != nil {
		return err
	}
	files, kept, dropped, err := selectProvider(files, strings.TrimSpace(*provider))
	if err != nil {
		return err
	}
	// A template item is not parsed or reduced, but it has to be renderable: it
	// must hold template text, and it must be the only item that fills its file.
	if err := validateExecutorTemplates(files); err != nil {
		return err
	}
	kept = append(kept, templateItemKeys(files)...)
	if err := checkExecutorsSize(files); err != nil {
		return err
	}
	items := make(map[string]string, len(files))
	keys := make([]string, 0, len(files))
	bytes := 0
	for key, data := range files {
		items[key] = string(data)
		keys = append(keys, key)
		bytes += len(data)
	}
	sort.Strings(keys)
	out := executorsSetOutput{
		Variable:  boxExecutorsVariable,
		Files:     executorFilesOf(items),
		Executors: executorsOf(executorFilesOf(items)),
		Bytes:     bytes,
		Provider:  strings.TrimSpace(*provider),
		Keys:      kept,
		Dropped:   dropped,
	}
	if *dryRun {
		a.note("would write %d file(s) to the variable %s", len(keys), boxExecutorsVariable)
		a.printExecutorKeys(out.Files, out.Keys, out.Dropped)
		return nil
	}
	written, err := a.Nomad.VariableWrite(ctx, &api.Variable{
		Path:  boxExecutorsVariable,
		Items: items,
	})
	if err != nil {
		return wrapf(err, "Nomad rejected the variable %s", boxExecutorsVariable)
	}
	if written != nil {
		out.Files = executorFilesOf(written.Items)
		out.Executors = executorsOf(out.Files)
	}
	if a.JSON {
		return a.printJSON(&out)
	}
	a.note("wrote %d file(s), %s, to the variable %s", len(out.Files), humanBytes(int64(out.Bytes)), out.Variable)
	a.printExecutorKeys(out.Files, out.Keys, out.Dropped)
	a.note("next: a new box renders these into /run/box/secrets/; an existing box keeps the files it started with")
	return nil
}

// printExecutorKeys prints the stored files and the JSON keys they hold: the
// file names and key names are not secret, their values are never printed.
func (a *App) printExecutorKeys(files []ExecutorFile, keys []ExecutorKey, dropped []string) {
	byFile := map[string][]ExecutorKey{}
	for _, key := range keys {
		byFile[key.File] = append(byFile[key.File], key)
	}
	for _, f := range files {
		a.note("  %-44s %6d bytes", f.Key, f.Size)
		for _, key := range byFile[f.Key] {
			switch key.Kind {
			case "credential":
				a.note("    credential %s: %s", key.Name, orNone(key.Fields))
			case "provider":
				a.note("    provider   %s", key.Name)
			case "template":
				a.note("    template   reads a secret at render time")
			default:
				a.note("    %s (not JSON)", key.Kind)
			}
		}
	}
	for _, key := range dropped {
		a.note("  not stored: %s (only --provider is kept)", key)
	}
}

// cmdExecutorsShow lists the executor files of the variable, never the values.
func (a *App) cmdExecutorsShow(ctx context.Context, args []string) error {
	fs := newFlagSet("executors show")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	variable, err := a.executorsVariable(ctx)
	if err != nil {
		return err
	}
	out := executorsShowOutput{Variable: boxExecutorsVariable}
	if variable == nil {
		out.Warn = "the variable does not exist: no box gets executor credentials until `box executors set` writes it"
	} else {
		out.Exists = true
		out.Files = executorFilesOf(variable.Items)
		out.Executors = executorsOf(out.Files)
		out.Missing = missingExecutorFiles(variable.Items)
		out.Keys = storedKeys(variable.Items)
		for _, f := range out.Files {
			out.Bytes += f.Size
		}
	}
	if a.JSON {
		if !out.Exists {
			return notFoundf("the variable %s does not exist: no executor is configured", boxExecutorsVariable)
		}
		return a.printJSON(&out)
	}
	if !out.Exists {
		a.note("%s", out.Warn)
		return notFoundf("the variable %s does not exist", boxExecutorsVariable)
	}
	a.note("variable %s: %d file(s), %s", out.Variable, len(out.Files), humanBytes(int64(out.Bytes)))
	for _, name := range boxExecutorNames {
		var have []string
		for _, f := range out.Files {
			if f.Executor != name {
				continue
			}
			// A template item is marked: its name says it is rendered, and the
			// operator has to see that the variable holds a path, not a value.
			if f.Template {
				have = append(have, f.Path+" (template)")
				continue
			}
			have = append(have, f.Path)
		}
		if len(have) == 0 {
			a.note("  %-9s no files", name)
			continue
		}
		a.note("  %-9s %s", name, strings.Join(have, ", "))
	}
	for _, key := range out.Keys {
		switch key.Kind {
		case "credential":
			a.note("  %-9s provider %s (%s)", key.File, key.Name, orNone(key.Fields))
		case "provider":
			models := "none listed"
			if len(key.Models) > 0 {
				models = strings.Join(prefixed(key.Name, key.Models), ", ")
			}
			a.note("  %-9s provider %s, staged models: %s", key.File, key.Name, models)
		case "template":
			a.note("  %-9s template: Nomad reads the secret at render time", key.File)
		default:
			a.note("  %-9s not JSON", key.File)
		}
	}
	if hasProvider(out.Keys) {
		a.note("  a box asks its provider for the models its key may use when a session starts; the staged list is what it falls back to")
	}
	for _, key := range out.Missing {
		a.note("  missing  %s (boxd needs it to start that executor)", key)
	}
	return nil
}

// cmdExecutorsDelete removes the variable, so no box gets credentials.
func (a *App) cmdExecutorsDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("executors delete")
	yes := fs.Bool("yes", false, "delete without asking")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	if !*yes {
		ok, err := a.confirm(fmt.Sprintf("delete the variable %s? boxes created from now on get no executor credentials [y/N] ", boxExecutorsVariable))
		if err != nil {
			return err
		}
		if !ok {
			a.note("nothing was deleted")
			return nil
		}
	}
	if err := a.Nomad.VariableDelete(ctx, boxExecutorsVariable); err != nil {
		if isNotFoundErr(err) {
			return notFoundf("the variable %s does not exist", boxExecutorsVariable)
		}
		return wrapf(err, "Nomad could not delete the variable %s", boxExecutorsVariable)
	}
	a.note("deleted the variable %s", boxExecutorsVariable)
	return nil
}

// checkExecutorsSize refuses a set of files Nomad cannot hold, naming the
// largest ones: a package checkout or a node_modules tree lands here.
func checkExecutorsSize(files map[string][]byte) error {
	total := 0
	sizes := make([]ExecutorFile, 0, len(files))
	for key, data := range files {
		total += len(data)
		executor, file, _ := splitExecutorKey(key)
		sizes = append(sizes, ExecutorFile{Key: key, Executor: executor, Path: file, Size: len(data)})
	}
	if total <= boxExecutorsVariableLimit {
		return nil
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i].Size > sizes[j].Size })
	var largest []string
	for i, f := range sizes {
		if i == 4 {
			break
		}
		largest = append(largest, fmt.Sprintf("%s (%d bytes)", f.Key, f.Size))
	}
	return usagef("the files are %d KiB together and Nomad limits one variable to %d KiB. The largest are: %s. "+
		"Stage the executor's own files only, for example with deploy/executors-from-prime.sh",
		total/1024, boxExecutorsVariableLimit/1024, strings.Join(largest, ", "))
}

// missingExecutorFiles lists the required files the variable does not hold.
func missingExecutorFiles(items map[string]string) []string {
	var missing []string
	for _, name := range boxExecutorNames {
		if !hasExecutorFiles(items, name) {
			continue
		}
		for _, required := range boxExecutorRequired[name] {
			if !hasExecutorFile(items, name+"/"+required) {
				missing = append(missing, name+"/"+required)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// hasExecutorFile reports whether the variable fills one file, either as the
// file itself or as the template that renders to it: an "auth.json.tmpl" item
// is all a box needs to get an auth.json.
func hasExecutorFile(items map[string]string, key string) bool {
	if _, ok := items[key]; ok {
		return true
	}
	_, ok := items[key+executorTemplateSuffix]
	return ok
}

// hasExecutorFiles reports whether the variable holds anything for executor.
func hasExecutorFiles(items map[string]string, executor string) bool {
	for key := range items {
		if strings.HasPrefix(key, executor+"/") {
			return true
		}
	}
	return false
}

// prefixed names models the way --model takes them: <provider>/<model>.
func prefixed(provider string, models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, provider+"/"+m)
	}
	return out
}

// hasProvider reports whether any stored file is a provider definition.
func hasProvider(keys []ExecutorKey) bool {
	for _, k := range keys {
		if k.Kind == "provider" {
			return true
		}
	}
	return false
}
