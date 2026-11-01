package driver

import (
	"os"
	"os/user"
	"testing"
)

func TestRuntimeUserRejectsRootAndDoesNotCreateAccounts(t *testing.T) {
	if _, e := resolveRuntimeUser("root"); e == nil {
		t.Fatal("root Lima accepted")
	}
	if _, e := resolveRuntimeUser("nomad-lima-nonexistent-test-account"); e == nil {
		t.Fatal("unknown account accepted")
	}
	if os.Geteuid() != 0 {
		u, e := user.Current()
		if e != nil {
			t.Fatal(e)
		}
		r, e := resolveRuntimeUser(u.Username)
		if e != nil {
			t.Fatal(e)
		}
		if r.credential != nil {
			t.Fatal("unprivileged process requested identity switch")
		}
		if e = r.validateHome(t.TempDir()); e != nil {
			t.Fatal(e)
		}
	}
}
