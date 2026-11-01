package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCacheExactModeDespiteUmask(t *testing.T) {
	_, c := fixture(t)
	old := syscall.Umask(0077)
	defer syscall.Umask(old)
	if e := c.prepareDirectory(); e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(c.Dir)
	if st.Mode().Perm() != 0700 {
		t.Fatal(st.Mode())
	}
}
func TestCacheExistingPermissionsNotRepaired(t *testing.T) {
	_, c := fixture(t)
	if e := os.Mkdir(c.Dir, 0755); e != nil {
		t.Fatal(e)
	}
	os.Chmod(c.Dir, 0755)
	if c.prepareDirectory() == nil {
		t.Fatal("unsafe existing cache accepted")
	}
	st, _ := os.Stat(c.Dir)
	if st.Mode().Perm() != 0755 {
		t.Fatal("silently repaired")
	}
}
func TestCacheRejectsSymlinkDirectoryAndArtifact(t *testing.T) {
	v, c := fixture(t)
	target := t.TempDir()
	if e := os.Symlink(target, c.Dir); e != nil {
		t.Fatal(e)
	}
	if c.prepareDirectory() == nil {
		t.Fatal("symlink cache accepted")
	}
	os.Remove(c.Dir)
	p, e := c.Materialize(t.Context(), v)
	if e != nil {
		t.Fatal(e)
	}
	os.Remove(p)
	victim := filepath.Join(target, "victim")
	os.WriteFile(victim, []byte("unchanged"), 0444)
	os.Symlink(victim, p)
	if _, e = c.Materialize(t.Context(), v); e == nil {
		t.Fatal("symlink yaml accepted")
	}
	b, _ := os.ReadFile(victim)
	if string(b) != "unchanged" {
		t.Fatal("target modified")
	}
}
func TestRootRuntimeTraversal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires disposable root Linux test environment")
	}
	d, e := os.MkdirTemp("/tmp", "cache-runtime-test-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(d)
	os.Chmod(d, 0755)
	c := Cache{Dir: filepath.Join(d, "cache"), Runtime: &Access{UID: 65534, GID: 65534}}
	if e := c.prepareDirectory(); e != nil {
		t.Fatal(e)
	}
	st, _ := os.Stat(c.Dir)
	raw := st.Sys().(*syscall.Stat_t)
	if raw.Uid != 0 || raw.Gid != 65534 || st.Mode().Perm() != 0710 {
		t.Fatal("wrong traversal policy", st)
	}

	artifact := filepath.Join(c.Dir, "accepted.yaml")
	if e := os.WriteFile(artifact, []byte("validated\n"), 0444); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("/bin/sh", "-c", `test "$(cat "$1/accepted.yaml")" = validated && ! ls "$1" >/dev/null 2>&1 && ! touch "$1/forbidden" 2>/dev/null`, "test", c.Dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{65534}}}
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("runtime traversal/read or no-list/no-write failed: %v %s", e, out)
	}
	os.Chmod(c.Dir, 0700)
	if c.prepareDirectory() == nil {
		t.Fatal("existing inaccessible cache accepted")
	}
	st, _ = os.Stat(c.Dir)
	if st.Mode().Perm() != 0700 {
		t.Fatal("existing cache repaired")
	}
}
