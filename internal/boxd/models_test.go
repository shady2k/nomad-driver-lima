package boxd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gateway is an OpenAI-compatible /models that answers one key.
func gateway(t *testing.T, key string, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var data []map[string]string
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeTestJSON(t *testing.T, path string, v any) {
	t.Helper()
	require(t, os.MkdirAll(filepath.Dir(path), 0o700))
	b, err := json.Marshal(v)
	require(t, err)
	require(t, os.WriteFile(path, b, 0o600))
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// stagePrime writes prime's staged files into the secrets directory, the way
// the driver does, with one sized model in the definition.
func stagePrime(t *testing.T, cfg DaemonConfig, base string) {
	t.Helper()
	dir := filepath.Join(cfg.SecretsDir, "prime")
	writeTestJSON(t, filepath.Join(dir, "auth.json"), map[string]any{"gw": map[string]any{"type": "api_key", "key": "k-1"}})
	writeTestJSON(t, filepath.Join(dir, "settings.json"), map[string]any{})
	writeTestJSON(t, filepath.Join(dir, "models.json"), map[string]any{"providers": map[string]any{"gw": map[string]any{
		"baseUrl": base + "/v1",
		"api":     "openai-completions",
		"models": []any{
			map[string]any{"id": "sized", "name": "Sized", "contextWindow": 200000, "maxTokens": 32000},
			map[string]any{"id": "gone", "name": "Gone", "contextWindow": 1000},
		},
	}}})
}

// TestDiscoverModelsTakesTheKeysList: the provider decides which models the
// key may use; a staged entry keeps its size, a new model joins without one
// and the journal says so, a model the key lost is gone.
func TestDiscoverModelsTakesTheKeysList(t *testing.T) {
	cfg := testConfig(t)
	var mu sync.Mutex
	var logs []string
	cfg.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	srv := gateway(t, "k-1", "sized", "new-one")
	stagePrime(t, cfg, srv.URL)
	require(t, prepareExecutorDirs(cfg))

	models, ok := ExecutorModels(cfg, "prime")
	if !ok || strings.Join(models, ",") != "gw/new-one,gw/sized" {
		t.Fatalf("models = %v, %v", models, ok)
	}
	doc, err := readJSONObject(filepath.Join(cfg.executorsDir(), "prime", "models.json"))
	require(t, err)
	list := doc["providers"].(map[string]any)["gw"].(map[string]any)["models"].([]any)
	for _, m := range list {
		model := m.(map[string]any)
		if model["id"] == "sized" && model["contextWindow"] != float64(200000) {
			t.Fatalf("the staged size was lost: %v", model)
		}
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "offers 2 model(s) to this key: new-one, sized") {
		t.Fatalf("the journal does not name the models:\n%s", joined)
	}
	if !strings.Contains(joined, "no size for new-one") {
		t.Fatalf("the journal does not name the model without a size:\n%s", joined)
	}
	if strings.Contains(joined, "k-1") {
		t.Fatal("the key reached the journal")
	}

	// The staged file in the secrets directory is not rewritten.
	staged, err := readJSONObject(filepath.Join(cfg.SecretsDir, "prime", "models.json"))
	require(t, err)
	if n := len(staged["providers"].(map[string]any)["gw"].(map[string]any)["models"].([]any)); n != 2 {
		t.Fatalf("the staged definition changed: %d models", n)
	}
}

// TestDiscoverModelsKeepsTheStagedListWhenTheProviderCannotAnswer.
func TestDiscoverModelsKeepsTheStagedListWhenTheProviderCannotAnswer(t *testing.T) {
	cfg := testConfig(t)
	srv := gateway(t, "another-key", "sized")
	stagePrime(t, cfg, srv.URL)
	require(t, prepareExecutorDirs(cfg))
	models, ok := ExecutorModels(cfg, "prime")
	if !ok || strings.Join(models, ",") != "gw/gone,gw/sized" {
		t.Fatalf("models = %v, %v", models, ok)
	}
}

// TestNewSessionRefusesAModelTheKeyDoesNotHave.
func TestNewSessionRefusesAModelTheKeyDoesNotHave(t *testing.T) {
	cfg := testConfig(t)
	srv := gateway(t, "k-1", "sized")
	stagePrime(t, cfg, srv.URL)
	m := newManager(t, cfg)
	_, err := m.NewSession("prime", "gw/gone", "", "")
	if err == nil || !strings.Contains(err.Error(), "the models are: gw/sized") {
		t.Fatalf("err = %v", err)
	}
	if _, err := m.NewSession("prime", "gw/sized", "", ""); err != nil {
		t.Fatalf("a model the key has: %v", err)
	}
}

// addon writes an addon with a catalog command for provider gw.
func addon(t *testing.T, cfg DaemonConfig, script string) {
	t.Helper()
	dir := filepath.Join(cfg.AddonsDir, "gw-addon")
	writeTestJSON(t, filepath.Join(dir, "package.json"), map[string]any{
		"name": "gw-addon",
		"box":  map[string]any{"catalog": map[string]any{"providers": []string{"gw"}, "command": []string{"sh", "catalog.sh"}}},
	})
	require(t, os.WriteFile(filepath.Join(dir, "catalog.sh"), []byte(script), 0o755))
}

// TestDiscoverModelsAsksTheAddonCatalog: an addon that declares a catalog for
// the provider gives the list and the sizes; the key reaches it on stdin only.
func TestDiscoverModelsAsksTheAddonCatalog(t *testing.T) {
	cfg := testConfig(t)
	srv := gateway(t, "k-1", "plain")
	stagePrime(t, cfg, srv.URL)
	addon(t, cfg, `in=$(cat)
case "$in" in *'"apiKey":"k-1"'*) ;; *) echo "no key on stdin" >&2; exit 3;; esac
case "$*" in *k-1*) echo "key in argv" >&2; exit 4;; esac
echo '{"models":[{"id":"luna","name":"Luna","contextWindow":272000,"maxTokens":128000},{"id":"flash","contextWindow":1000000}]}'
`)
	require(t, prepareExecutorDirs(cfg))
	models, ok := ExecutorModels(cfg, "prime")
	if !ok || strings.Join(models, ",") != "gw/flash,gw/luna" {
		t.Fatalf("models = %v, %v", models, ok)
	}
	doc, err := readJSONObject(filepath.Join(cfg.executorsDir(), "prime", "models.json"))
	require(t, err)
	for _, m := range doc["providers"].(map[string]any)["gw"].(map[string]any)["models"].([]any) {
		model := m.(map[string]any)
		if model["id"] == "luna" && model["contextWindow"] != float64(272000) {
			t.Fatalf("the addon's size was not kept: %v", model)
		}
	}
}

// TestDiscoverModelsFallsBackWhenTheCatalogFails: a catalog that fails leaves
// the provider's own list.
func TestDiscoverModelsFallsBackWhenTheCatalogFails(t *testing.T) {
	cfg := testConfig(t)
	srv := gateway(t, "k-1", "sized")
	stagePrime(t, cfg, srv.URL)
	addon(t, cfg, "cat >/dev/null; echo broken >&2; exit 1\n")
	require(t, prepareExecutorDirs(cfg))
	models, _ := ExecutorModels(cfg, "prime")
	if strings.Join(models, ",") != "gw/sized" {
		t.Fatalf("models = %v", models)
	}
}
