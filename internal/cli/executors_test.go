package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// executorDir writes a staging directory with one subdirectory per executor.
func executorDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

func TestExecutorTemplatesOnePerFile(t *testing.T) {
	spec := goodSpec()
	spec.ExecutorItems = []ExecutorItem{
		{Key: "prime/auth.json"}, {Key: "prime/settings.json"}, {Key: "opencode/opencode.json"},
		{Key: "not-an-executor/x"},
	}
	job, err := RenderJob(spec)
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 3, "the key of an unknown executor is dropped")

	first := task.Templates[0]
	require.Equal(t, "secrets/opencode/opencode.json", derefStr(first.DestPath, ""))
	require.Equal(t, "noop", derefStr(first.ChangeMode, ""), "reading a box must not restart its VM")
	require.Equal(t, "0600", derefStr(first.Perms, ""))
	require.Contains(t, derefStr(first.EmbeddedTmpl, ""), "nomadVarExists")
	require.Contains(t, derefStr(first.EmbeddedTmpl, ""), boxExecutorsVariable)
	require.Contains(t, derefStr(first.EmbeddedTmpl, ""), `(index . "opencode/opencode.json").Value`)
}

// vaultAuthTemplate is what deploy/executors-from-prime.sh writes for the
// prime executor: the provider entry with the key read from Vault at render
// time. The text holds a path, never a credential.
const vaultAuthTemplate = `{"gateway": {"type": "api_key", "key": {{ with secret "kv/data/box/llm" }}{{ .Data.data.key | toJSON }}{{ end }}}}`

func TestExecutorTemplatesEmbedATemplateItem(t *testing.T) {
	spec := goodSpec()
	spec.ExecutorItems = []ExecutorItem{
		{Key: "prime/auth.json.tmpl", Text: vaultAuthTemplate},
		{Key: "prime/settings.json"},
	}
	job, err := RenderJob(spec)
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 2)

	auth := task.Templates[0]
	require.Equal(t, "secrets/prime/auth.json", derefStr(auth.DestPath, ""),
		"a .tmpl item renders to the same file as the plain item would")
	require.Equal(t, vaultAuthTemplate, derefStr(auth.EmbeddedTmpl, ""), "the item's own text is the template")
	require.NotContains(t, derefStr(auth.EmbeddedTmpl, ""), boxExecutorsVariable,
		"a template item is not read out of the variable")
	require.Equal(t, "0600", derefStr(auth.Perms, ""))
	require.Equal(t, "noop", derefStr(auth.ChangeMode, ""))

	plain := task.Templates[1]
	require.Equal(t, "secrets/prime/settings.json", derefStr(plain.DestPath, ""))
	require.Contains(t, derefStr(plain.EmbeddedTmpl, ""), "nomadVarExists")
	require.Contains(t, derefStr(plain.EmbeddedTmpl, ""), `(index . "prime/settings.json").Value`)
}

func TestCreateRendersATemplateItemWithAVaultBlock(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.cluster.setExecutors(map[string]string{
		"prime/auth.json.tmpl": vaultAuthTemplate,
		"prime/settings.json":  "{}",
	})

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0", "--vault-role", "box-workloads")
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 2)
	require.NotNil(t, task.Vault, "a template item reads Vault, so the task needs a Vault block")
	require.Equal(t, "box-workloads", task.Vault.Role)
	require.Equal(t, "noop", derefStr(task.Vault.ChangeMode, ""), "a renewed token must not restart the VM")
	require.True(t, *task.Vault.DisableFile,
		"Nomad must not write a token into secrets/: the driver copies that directory into the guest")
	require.False(t, *task.Vault.Env, "the task process is the VM, not an executor")
}

func TestCreateTakesTheVaultRoleFromTheEnvironment(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.SetVaultRole("box-workloads")
	ta.cluster.setExecutors(map[string]string{"prime/auth.json.tmpl": vaultAuthTemplate})

	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.NotNil(t, task.Vault)
	require.Equal(t, "box-workloads", task.Vault.Role, "BOX_VAULT_ROLE is the default of --vault-role")

	// A start rebuilds the job, and the role the box was created with is kept.
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"), ta.errOut.String())
	ta.SetVaultRole("other-role")
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())
	job, err = ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err = boxTask(job)
	require.NoError(t, err)
	require.Equal(t, "box-workloads", task.Vault.Role, "start keeps the role the box was created with")
}

func TestStartPicksUpATemplateItemAndTheRole(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"), ta.errOut.String())

	ta.SetVaultRole("box-workloads")
	ta.cluster.setExecutors(map[string]string{"prime/auth.json.tmpl": vaultAuthTemplate})
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 1)
	require.Equal(t, "secrets/prime/auth.json", derefStr(task.Templates[0].DestPath, ""))
	require.NotNil(t, task.Vault, "a box created without a template item gets its Vault block at its next start")
	require.Equal(t, "box-workloads", task.Vault.Role)
}

func TestCreateRendersTheExecutorFiles(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	ta.cluster.setExecutors(map[string]string{
		"prime/auth.json":     "{}",
		"prime/settings.json": "{}",
	})

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 2)
	// The job carries file names, never the values.
	require.NotContains(t, job.Meta, "prime/auth.json")
}

func TestCreateWithoutExecutorsVariableStartsAnUnconfiguredBox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }

	rc := ta.run(t, "create", "dev1", "--image", "1.0.0")
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Empty(t, task.Templates)
	require.Nil(t, task.Vault, "no executor item means no Vault block")
}

func TestStartPicksUpTheExecutorsVariable(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())
	require.Equal(t, ExitOK, ta.run(t, "stop", "dev1"), ta.errOut.String())

	ta.cluster.setExecutors(map[string]string{"opencode/opencode.json": "{}"})
	require.Equal(t, ExitOK, ta.run(t, "start", "dev1"), ta.errOut.String())

	job, err := ta.cluster.Job(t.Context(), BoxJobName("dev1"))
	require.NoError(t, err)
	task, err := boxTask(job)
	require.NoError(t, err)
	require.Len(t, task.Templates, 1)
	require.Equal(t, "secrets/opencode/opencode.json", derefStr(task.Templates[0].DestPath, ""))
}

func TestExecutorsSetAndShow(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"prime/settings.json":    `{"packages":["https://git.example/pi-gateway.git"]}`,
		"prime/auth.json":        `{"gateway":{"type":"oauth","access":"tok-gateway"},"other":{"type":"api","key":"tok-other"}}`,
		"opencode/opencode.json": `{"providers":{"gateway":{"name":"Gateway"},"other":{"name":"Other"}}}`,
		"opencode/env":           "GATEWAY_API_KEY=secret\n",
	})
	rc := ta.run(t, "executors", "set", "--from-dir", dir, "--provider", "gateway")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "wrote 4 file(s)")
	require.NotContains(t, ta.errOut.String(), "GATEWAY_API_KEY=", "an api key must never be printed")
	require.NotContains(t, ta.errOut.String(), "tok-", "no credential value is ever printed")

	variable, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.NoError(t, err)
	require.Len(t, variable.Items, 4)
	require.Equal(t, "GATEWAY_API_KEY=secret\n", variable.Items["opencode/env"])

	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "prime")
	require.Contains(t, ta.errOut.String(), "auth.json, settings.json")
	require.Contains(t, ta.errOut.String(), "provider gateway (access, type)")
	require.NotContains(t, ta.errOut.String(), "GATEWAY_API_KEY=")
	require.NotContains(t, ta.errOut.String(), "tok-")
}

func TestExecutorsSetStoresATemplateItemAsItIs(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"prime/auth.json.tmpl": vaultAuthTemplate,
		"prime/settings.json":  "{}",
	})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "template")

	variable, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.NoError(t, err)
	require.Equal(t, vaultAuthTemplate, variable.Items["prime/auth.json.tmpl"], "the template text is stored unchanged")

	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "auth.json.tmpl (template)")
	require.Contains(t, ta.errOut.String(), "template: Nomad reads the secret at render time")
	require.NotContains(t, ta.errOut.String(), "missing", "auth.json.tmpl satisfies the required auth.json")
}

func TestExecutorsSetRefusesATemplateItemWithoutTemplateText(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{"prime/auth.json.tmpl": `{"gateway": {"type": "api_key", "key": "tok-secret"}}`})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "holds no template text")
	require.Contains(t, ta.errOut.String(), "prime/auth.json.tmpl")
	require.NotContains(t, ta.errOut.String(), "tok-secret", "the message never prints what the item holds")
	_, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.Error(t, err, "nothing is stored")
}

func TestExecutorsSetRefusesTwoItemsRenderingToOneFile(t *testing.T) {
	// "prime/auth.json" and "prime/auth.json.tmpl" both write secrets/prime/auth.json:
	// which one a box gets would be the order Nomad renders them in.
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"prime/auth.json":      `{"gateway": {"type": "oauth"}}`,
		"prime/auth.json.tmpl": vaultAuthTemplate,
	})
	rc := ta.run(t, "executors", "set", "--from-dir", dir, "--provider", "gateway")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "both render to prime/auth.json")
	require.Contains(t, ta.errOut.String(), "prime/auth.json")
	require.Contains(t, ta.errOut.String(), "prime/auth.json.tmpl")
}

func TestExecutorsShowReportsATemplateItemForARequiredFile(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setExecutors(map[string]string{
		"prime/settings.json":  "{}",
		"prime/auth.json.tmpl": vaultAuthTemplate,
	})
	rc := ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.NotContains(t, ta.errOut.String(), "missing")

	ta.JSON = true
	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	var doc executorsShowOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Empty(t, doc.Missing)
	require.True(t, doc.Files[0].Template)
	require.Equal(t, "prime/auth.json.tmpl", doc.Files[0].Key)
}

func TestExecutorsSetKeepsOnlyTheChosenProvider(t *testing.T) {
	// The whole auth.json and every provider of models.json are what C6 is
	// about: only the chosen provider travels.
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"prime/auth.json":        `{"gateway":{"type":"oauth","access":"tok-gateway"},"other":{"type":"api","key":"tok-other"}}`,
		"prime/models.json":      `{"schema":1,"providers":{"gateway":{"baseUrl":"https://one.example/v1"},"other":{"baseUrl":"https://two.example/v1"}}}`,
		"prime/settings.json":    `{"defaultProvider":"gateway"}`,
		"opencode/opencode.json": `{"model":"gateway/m1","providers":{"gateway":{"name":"Gateway"},"other":{"name":"Other"}}}`,
	})
	rc := ta.run(t, "executors", "set", "--from-dir", dir, "--provider", "gateway")
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	variable, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.NoError(t, err)

	var auth map[string]any
	require.NoError(t, json.Unmarshal([]byte(variable.Items["prime/auth.json"]), &auth))
	require.Equal(t, []string{"gateway"}, mapKeys(auth), "only the chosen credential is stored")
	require.NotContains(t, variable.Items["prime/auth.json"], "tok-other")

	var models map[string]any
	require.NoError(t, json.Unmarshal([]byte(variable.Items["prime/models.json"]), &models))
	require.Equal(t, []string{"gateway"}, mapKeys(models["providers"].(map[string]any)))
	require.Equal(t, float64(1), models["schema"], "the rest of the document is kept")

	var opencode map[string]any
	require.NoError(t, json.Unmarshal([]byte(variable.Items["opencode/opencode.json"]), &opencode))
	require.Equal(t, []string{"gateway"}, mapKeys(opencode["providers"].(map[string]any)))

	// The key names are printed, the values never.
	require.Contains(t, ta.errOut.String(), "credential gateway: access, type")
	require.Contains(t, ta.errOut.String(), "provider   gateway")
	require.Contains(t, ta.errOut.String(), "not stored: prime/auth.json: other")
	require.Contains(t, ta.errOut.String(), "not stored: prime/models.json: other")
	require.NotContains(t, ta.errOut.String(), "tok-")
}

func TestExecutorsSetNeedsTheProvider(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{"prime/auth.json": `{"gateway":{"type":"oauth"}}`})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "--provider")
	require.Contains(t, ta.errOut.String(), "prime/auth.json")
	_, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.Error(t, err, "nothing is stored without a provider")
}

func TestExecutorsSetRefusesAnUnknownProvider(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"prime/auth.json":   `{"gateway":{"type":"oauth"}}`,
		"prime/models.json": `{"providers":{"gateway":{"baseUrl":"https://one.example/v1"}}}`,
	})
	rc := ta.run(t, "executors", "set", "--from-dir", dir, "--provider", "absent")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "no credential for \"absent\"")
	require.Contains(t, ta.errOut.String(), "gateway")

	// models.json without that provider is refused too.
	dir2 := executorDir(t, map[string]string{
		"prime/models.json": `{"providers":{"gateway":{"baseUrl":"https://one.example/v1"}}}`,
	})
	ta.out.Reset()
	ta.errOut.Reset()
	rc = ta.run(t, "executors", "set", "--from-dir", dir2, "--provider", "absent")
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "does not define the provider \"absent\"")
}

func TestExecutorsSetWithoutProviderScopedFiles(t *testing.T) {
	// A claude-only set has no provider in it, so no --provider is needed.
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{"claude/credentials.json": `{"token":"x"}`})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitOK, rc, ta.errOut.String())
}

// mapKeys lists the keys of a decoded JSON object, sorted.
func mapKeys(doc map[string]any) []string {
	out := make([]string, 0, len(doc))
	for key := range doc {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func TestExecutorsSetOneExecutor(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"settings.json":     "{}",
		"auth.json":         `{"gateway":{"type":"oauth"}}`,
		"git/host/pkg/x.js": "export default 1",
	})
	rc := ta.run(t, "executors", "set", "--executor", "prime", "--from-dir", dir, "--provider", "gateway")
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	variable, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.NoError(t, err)
	require.Contains(t, variable.Items, "prime/git/host/pkg/x.js")
	require.Contains(t, variable.Items, "prime/settings.json")
}

func TestExecutorsSetSkipsPackagesAndHiddenFiles(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{
		"settings.json":             "{}",
		".git/config":               "checkout",
		"node_modules/pkg/index.js": "installed",
		"git/host/pkg/index.js":     "extension",
	})
	rc := ta.run(t, "executors", "set", "--executor", "prime", "--from-dir", dir)
	require.Equal(t, ExitOK, rc, ta.errOut.String())

	variable, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"prime/settings.json":         "{}",
		"prime/git/host/pkg/index.js": "extension",
	}, map[string]string(variable.Items))
}

func TestExecutorsSetRejectsAnUnknownExecutor(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	dir := executorDir(t, map[string]string{"nope/x.json": "{}"})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "not an executor boxd knows")
}

func TestExecutorsSetNeedsADirectory(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "executors", "set"))
	require.Contains(t, ta.errOut.String(), "--from-dir")
}

func TestExecutorsShowWithoutTheVariable(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	rc := ta.run(t, "executors", "show")
	require.Equal(t, ExitNotFound, rc)
	require.Contains(t, ta.errOut.String(), "does not exist")
}

func TestExecutorsShowReportsMissingRequiredFiles(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setExecutors(map[string]string{"prime/settings.json": "{}"})
	rc := ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	require.Contains(t, ta.errOut.String(), "missing")
	require.Contains(t, ta.errOut.String(), "prime/auth.json")
}

func TestExecutorsShowJSON(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setExecutors(map[string]string{
		"prime/settings.json": "{}",
		"prime/auth.json":     "{}",
	})
	ta.JSON = true
	rc := ta.run(t, "executors", "show")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	var doc executorsShowOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.True(t, doc.Exists)
	require.Equal(t, []string{"prime"}, doc.Executors)
	require.Len(t, doc.Files, 2)
	require.Empty(t, doc.Missing)
}

func TestExecutorsDelete(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setExecutors(map[string]string{"prime/auth.json": "{}"})
	rc := ta.run(t, "executors", "delete", "--yes")
	require.Equal(t, ExitOK, rc, ta.errOut.String())
	_, err := ta.cluster.VariableRead(t.Context(), boxExecutorsVariable)
	require.Error(t, err)
}

func TestStatusShowsTheExecutorsOfABox(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.setExecutors(map[string]string{"prime/auth.json": "{}", "prime/settings.json": "{}"})
	ta.cluster.onRegister = func(c *fakeCluster, job *api.Job) { c.runBox(job) }
	require.Equal(t, ExitOK, ta.run(t, "create", "dev1", "--image", "1.0.0"), ta.errOut.String())

	ta.out.Reset()
	ta.errOut.Reset()
	ta.JSON = true
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"), ta.errOut.String())
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, []string{"prime"}, doc.Spec.Executors)
	require.Equal(t, []string{"prime/auth.json", "prime/settings.json"}, doc.Spec.ExecutorFiles)
}

func TestExecutorsSetRefusesAVariableNomadCannotHold(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	big := strings.Repeat("x", boxExecutorsVariableLimit+1)
	dir := executorDir(t, map[string]string{"prime/big.json": big})
	rc := ta.run(t, "executors", "set", "--from-dir", dir)
	require.Equal(t, ExitUsage, rc)
	require.Contains(t, ta.errOut.String(), "64 KiB")
	require.Contains(t, ta.errOut.String(), "prime/big.json")
}
