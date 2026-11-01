package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryPairInputsStrictPrivateAndNoSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runtime.config")
	raw := []byte(`{"state_dir":"/example/state","lima_home":"/example/home","limactl":"/example/limactl","runtime_user":"worker","vault_address":"https://secrets.example.invalid","expected_version":"2.2.1"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var cfg pairRuntimeConfig
	if _, err := readPairInput(path, &cfg); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	os.Symlink(path, link)
	if _, err := readPairInput(link, &cfg); err == nil {
		t.Fatal("symlink input accepted")
	}
	os.Chmod(path, 0644)
	if _, err := readPairInput(path, &cfg); err == nil {
		t.Fatal("non-private input accepted")
	}
	os.Chmod(path, 0600)
	os.WriteFile(path, []byte(`{"vault_token":"synthetic-not-permitted"}`), 0600)
	if _, err := readPairInput(path, &cfg); err == nil {
		t.Fatal("unknown secret config accepted")
	}
}
