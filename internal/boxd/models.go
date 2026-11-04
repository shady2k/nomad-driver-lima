package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The models a box can use are the ones its provider offers to its key. The
// cluster's variable stages a provider definition (where the provider is, its
// API) and a model list as it was when the operator staged it; boxd asks the
// provider itself, with the key Nomad rendered into the tmpfs, and rewrites the
// list in its own copy of the configuration. A model added to the key is
// there at the next session, a model taken away is gone, and the list never
// comes from anyone's host.
//
// prime-agent 0.10.2 reads its providers from models.json only: a provider an
// extension registers is not used over ACP, so the list has to be in the file.

// modelsTimeout bounds one provider's answer: a session waits for it.
const modelsTimeout = 10 * time.Second

// discoverModels rewrites the model lists of prime's models.json and
// opencode's opencode.json from what each provider offers to its key. A
// provider that cannot be asked keeps its staged list, and the journal says so.
func discoverModels(cfg DaemonConfig) {
	root := cfg.executorsDir()
	primeDir := filepath.Join(root, "prime")
	if dirExists(primeDir) {
		keys := primeKeys(filepath.Join(primeDir, "auth.json"))
		rewriteModels(cfg, "prime", filepath.Join(primeDir, "models.json"), keys, primeModelList)
	}
	ocFile := filepath.Join(cfg.dirsOf("opencode").config, "opencode", "opencode.json")
	if fileExists(ocFile) {
		env, _ := readEnvFile(filepath.Join(cfg.SecretsDir, "opencode", "env"))
		rewriteModels(cfg, "opencode", ocFile, opencodeKeys(ocFile, env), opencodeModelList)
	}
}

// modelList reads and writes the model list of one provider definition, in
// the shape its executor keeps it.
type modelList struct {
	// baseURL is where the provider's /models is.
	baseURL func(def map[string]any) string
	// get returns the staged models by id, set replaces them.
	get func(def map[string]any) map[string]any
	set func(def map[string]any, ids []string, staged map[string]any)
	// sized reports whether a staged model says how large its context is:
	// a provider's /models does not, so only the staged entry can.
	sized func(model any) bool
	// setCatalog replaces the models with an addon's catalog, which is in
	// prime's shape (see catalogModels).
	setCatalog func(def map[string]any, models []map[string]any, staged map[string]any)
}

// primeModelList: prime keeps "baseUrl" and "models": [{"id", "name", ...}].
var primeModelList = modelList{
	baseURL: func(def map[string]any) string { s, _ := def["baseUrl"].(string); return s },
	get: func(def map[string]any) map[string]any {
		out := map[string]any{}
		list, _ := def["models"].([]any)
		for _, m := range list {
			if model, ok := m.(map[string]any); ok {
				if id, ok := model["id"].(string); ok {
					out[id] = model
				}
			}
		}
		return out
	},
	set: func(def map[string]any, ids []string, staged map[string]any) {
		list := make([]any, 0, len(ids))
		for _, id := range ids {
			if m, ok := staged[id]; ok {
				list = append(list, m)
				continue
			}
			list = append(list, map[string]any{"id": id, "name": id})
		}
		def["models"] = list
	},
	sized: func(model any) bool {
		m, _ := model.(map[string]any)
		_, ok := m["contextWindow"]
		return ok
	},
	setCatalog: func(def map[string]any, models []map[string]any, _ map[string]any) {
		list := make([]any, 0, len(models))
		for _, m := range models {
			list = append(list, m)
		}
		def["models"] = list
	},
}

// opencodeModelList: opencode keeps "settings.baseURL" (or "options.baseURL")
// and "models": {"<id>": {...}}.
var opencodeModelList = modelList{
	baseURL: func(def map[string]any) string {
		for _, key := range []string{"settings", "options"} {
			if m, ok := def[key].(map[string]any); ok {
				if s, ok := m["baseURL"].(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	},
	get: func(def map[string]any) map[string]any {
		out := map[string]any{}
		if models, ok := def["models"].(map[string]any); ok {
			for id, m := range models {
				out[id] = m
			}
		}
		return out
	},
	set: func(def map[string]any, ids []string, staged map[string]any) {
		models := make(map[string]any, len(ids))
		for _, id := range ids {
			if m, ok := staged[id]; ok {
				models[id] = m
				continue
			}
			models[id] = map[string]any{}
		}
		def["models"] = models
	},
	sized: func(model any) bool {
		m, _ := model.(map[string]any)
		limit, _ := m["limit"].(map[string]any)
		_, ok := limit["context"]
		return ok
	},
	// opencode names a model's sizes limit.context and limit.output.
	setCatalog: func(def map[string]any, models []map[string]any, staged map[string]any) {
		out := make(map[string]any, len(models))
		for _, m := range models {
			id, _ := m["id"].(string)
			entry := map[string]any{}
			if old, ok := staged[id].(map[string]any); ok {
				for k, v := range old {
					entry[k] = v
				}
			}
			if name, ok := m["name"].(string); ok && name != "" {
				entry["name"] = name
			}
			limit := map[string]any{}
			if old, ok := entry["limit"].(map[string]any); ok {
				for k, v := range old {
					limit[k] = v
				}
			}
			if v, ok := m["contextWindow"]; ok {
				limit["context"] = v
			}
			if v, ok := m["maxTokens"]; ok {
				limit["output"] = v
			}
			if len(limit) > 0 {
				entry["limit"] = limit
			}
			out[id] = entry
		}
		def["models"] = out
	},
}

// rewriteModels asks every provider of one configuration file for its models
// and writes the file back when any list changed.
func rewriteModels(cfg DaemonConfig, executor, path string, keys map[string]string, shape modelList) {
	doc, err := readJSONObject(path)
	if err != nil {
		cfg.logf("%s: cannot read %s, its model list is left as staged: %v", executor, filepath.Base(path), err)
		return
	}
	providers, _ := doc["providers"].(map[string]any)
	changed := false
	for _, name := range sortedKeys(providers) {
		def, ok := providers[name].(map[string]any)
		if !ok {
			continue
		}
		base := shape.baseURL(def)
		key := keys[name]
		if base == "" || key == "" {
			continue
		}
		staged := shape.get(def)
		if addon, models, err := catalogModels(cfg, name, base, key); addon != "" {
			if err == nil {
				shape.setCatalog(def, models, staged)
				changed = true
				cfg.logf("%s: provider %s offers %d model(s) to this key, sized by the %s addon: %s",
					executor, name, len(models), addon, strings.Join(modelIDsOf(models), ", "))
				continue
			}
			cfg.logf("%s: the %s addon could not list provider %s's models, asking the provider: %v", executor, addon, name, err)
		}
		ids, err := providerModels(base, key)
		if err != nil {
			cfg.logf("%s: provider %s did not list its models, %d staged model(s) kept: %v", executor, name, len(staged), err)
			continue
		}
		shape.set(def, ids, staged)
		changed = true
		cfg.logf("%s: provider %s offers %d model(s) to this key: %s", executor, name, len(ids), strings.Join(ids, ", "))
		var unsized []string
		for _, id := range ids {
			if m, ok := staged[id]; !ok || !shape.sized(m) {
				unsized = append(unsized, id)
			}
		}
		if len(unsized) > 0 {
			cfg.logf("%s: provider %s does not say how large a model's context is, and the staged definition has no size for %s: "+
				"%s assumes its own default there; the operator adds the size to the provider definition (`box executors set`)",
				executor, name, strings.Join(unsized, ", "), executor)
		}
	}
	if !changed {
		return
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(b, '\n'), 0o600)
	}
	if err != nil {
		cfg.logf("%s: cannot write the model list to %s: %v", executor, filepath.Base(path), err)
	}
}

// providerModels asks an OpenAI-compatible provider which models the key may
// use: GET <base>/models, the ids of its data array, sorted.
func providerModels(base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode the answer: %w", err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range body.Data {
		id := strings.TrimSpace(m.ID)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("the answer names no model")
	}
	sort.Strings(ids)
	return ids, nil
}

// primeKeys reads the key of every provider in prime's auth.json: an api_key
// credential's key, or an oauth credential's access token.
func primeKeys(path string) map[string]string {
	out := map[string]string{}
	doc, err := readJSONObject(path)
	if err != nil {
		return out
	}
	for name, v := range doc {
		cred, _ := v.(map[string]any)
		if key, ok := cred["key"].(string); ok && key != "" {
			out[name] = key
		} else if access, ok := cred["access"].(string); ok && access != "" {
			out[name] = access
		}
	}
	return out
}

// opencodeKeys finds each opencode provider's key in the env file: the first
// variable its "env" list names that the file sets.
func opencodeKeys(path string, env []string) map[string]string {
	out := map[string]string{}
	doc, err := readJSONObject(path)
	if err != nil {
		return out
	}
	values := map[string]string{}
	for _, line := range env {
		if k, v, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	providers, _ := doc["providers"].(map[string]any)
	for name, v := range providers {
		def, _ := v.(map[string]any)
		names, _ := def["env"].([]any)
		for _, n := range names {
			if s, ok := n.(string); ok && values[s] != "" {
				out[name] = values[s]
				break
			}
		}
	}
	return out
}

// ExecutorModels lists the models a session of executor may name, as
// <provider>/<model>, from its configuration on the tmpfs. ok is false when the
// executor has no model list, and then any model is left to the executor.
func ExecutorModels(cfg DaemonConfig, executor string) (models []string, ok bool) {
	var path string
	var shape modelList
	switch executor {
	case "prime":
		path, shape = filepath.Join(cfg.executorsDir(), "prime", "models.json"), primeModelList
	case "opencode":
		path, shape = filepath.Join(cfg.dirsOf("opencode").config, "opencode", "opencode.json"), opencodeModelList
	default:
		return nil, false
	}
	doc, err := readJSONObject(path)
	if err != nil {
		return nil, false
	}
	providers, _ := doc["providers"].(map[string]any)
	for _, name := range sortedKeys(providers) {
		def, _ := providers[name].(map[string]any)
		for _, id := range sortedKeys(shape.get(def)) {
			models = append(models, name+"/"+id)
		}
	}
	return models, len(models) > 0
}

func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// An addon in the image can know more about a provider's models than the
// provider says: their context size above all, which an OpenAI-compatible
// /models does not carry. Such an addon declares a catalog command in its
// package.json:
//
//	"box": {"catalog": {"providers": ["myprovider"], "command": ["node", "box-catalog.mjs"]}}
//
// boxd runs the command in the addon's directory with one JSON object on
// stdin, {"provider", "baseUrl", "apiKey"} (the key never goes into argv or
// the environment), and reads {"models": [...]} from stdout: prime's model
// entries, each with an id and, where the addon knows them, contextWindow,
// maxTokens, reasoning and the rest. The answer is the list, sizes included.

// catalogTimeout bounds one catalog command, which usually asks the provider.
const catalogTimeout = 20 * time.Second

// catalogLimit bounds what a catalog command may print.
const catalogLimit = 1 << 20

type addonCatalog struct {
	Box struct {
		Catalog struct {
			Providers []string `json:"providers"`
			Command   []string `json:"command"`
		} `json:"catalog"`
	} `json:"box"`
}

// catalogModels runs the catalog command of the addon that declares provider,
// if any. addon is "" when no addon declares it.
func catalogModels(cfg DaemonConfig, provider, base, key string) (addon string, models []map[string]any, err error) {
	dir, command := findCatalog(cfg.addonsDir(), provider)
	if dir == "" {
		return "", nil, nil
	}
	addon = filepath.Base(dir)
	in, err := json.Marshal(map[string]string{"provider": provider, "baseUrl": base, "apiKey": key})
	if err != nil {
		return addon, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(in)
	var out, errOut limitedBuffer
	out.limit, errOut.limit = catalogLimit, 4096
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return addon, nil, fmt.Errorf("%v: %s", err, oneLine(errOut.String(), 200))
	}
	var answer struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		return addon, nil, fmt.Errorf("the answer is not {\"models\": [...]}: %w", err)
	}
	for _, m := range answer.Models {
		if id, ok := m["id"].(string); ok && strings.TrimSpace(id) != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return addon, nil, fmt.Errorf("the answer names no model")
	}
	return addon, models, nil
}

// findCatalog finds the addon that declares a catalog for provider.
func findCatalog(root, provider string) (dir string, command []string) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", nil
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(root, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pkg addonCatalog
		if json.Unmarshal(b, &pkg) != nil || len(pkg.Box.Catalog.Command) == 0 {
			continue
		}
		if containsString(pkg.Box.Catalog.Providers, provider) {
			return d, pkg.Box.Catalog.Command
		}
	}
	return "", nil
}

func modelIDsOf(models []map[string]any) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// limitedBuffer keeps the first limit bytes written to it.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
