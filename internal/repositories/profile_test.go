package repositories

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileCanonicalMatchesGuestAndRejectsUnsafeSettings(t *testing.T) {
	raw := []byte(`{"repository_profile_default":"cache","repository_profiles":{"cache":{"schema_version":1,"id":"cache","version":"v1","architectures":["amd64"],"apt":{"suite_uris":{"noble":"https://cache.example.invalid/apt"},"components":["main"]},"npm":{"registry":"https://cache.example.invalid/npm"},"go":{"proxy":"https://cache.example.invalid/go"}}}}`)
	path := filepath.Join(t.TempDir(), "profiles.json")
	os.WriteFile(path, raw, 0600)
	c, e := Load(path, Hash(raw))
	if e != nil {
		t.Fatal(e)
	}
	s, e := Select(c, "", "")
	if e != nil {
		t.Fatal(e)
	}
	canonical, _ := Canonical(s.Profile)
	script := `import json,sys,importlib.util;spec=importlib.util.spec_from_file_location('r','../../plugin/guest/repository_profile.py');p=importlib.util.module_from_spec(spec);spec.loader.exec_module(p);v=p.validate(json.load(sys.stdin),{'os':'linux','distribution':'ubuntu','version':'24.04','architecture':'amd64'});sys.stdout.buffer.write(p.canonical(v))`
	cmd := exec.Command("python3", "-c", script)
	cmd.Stdin = bytes.NewReader(canonical)
	out, e := cmd.Output()
	if e != nil || !bytes.Equal(out, canonical) {
		t.Fatal(e, "Go/guest canonical mismatch", string(out), string(canonical))
	}
	for _, bad := range []string{strings.Replace(string(raw), "https://cache.example.invalid/npm", "http://cache.example.invalid/npm", 1), strings.Replace(string(raw), `"registry":`, `"token":"secret","registry":`, 1)} {
		os.WriteFile(path, []byte(bad), 0600)
		if _, e = Load(path, Hash([]byte(bad))); e == nil {
			t.Fatal("unsafe profile accepted")
		}
	}
	if v, e := Select(Config{}, "", ""); e != nil || v != nil {
		t.Fatal("disabled behavior changed")
	}
	if _, e = Select(c, "missing", ""); e == nil {
		t.Fatal("unknown ID accepted")
	}
	var stored Selection
	encoded, _ := json.Marshal(s)
	json.Unmarshal(encoded, &stored)
	if e = stored.Validate(); e != nil {
		t.Fatal(e)
	}
	stored.Profile.Go.Proxy = "https://different.example.invalid"
	if stored.Validate() == nil {
		t.Fatal("changed durable profile accepted")
	}
}
