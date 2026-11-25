package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSealStatusAnswersWithoutChangingAnything: `box bake` asks the box whether
// the seal would run before it stops it, so a busy box is refused with no stop.
func TestSealStatusAnswersWithoutChangingAnything(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)

	if st := m.SealStatus(); !st.OK || st.Reason != "" {
		t.Fatalf("an idle box would not be sealed: %+v", st)
	}
	// Nothing was deleted by the question.
	if !g.exists("work/project/main.go") {
		t.Fatal("asking whether a seal may run deleted the workspace")
	}

	s := mustSession(t, m, "prime", "acme/model")
	sess := m.lookupSession(s.ID)
	sess.mu.Lock()
	sess.busy = true
	sess.cur = &turnState{Turn: Turn{Session: s.ID, Turn: 2, State: TurnRunning}, done: make(chan struct{})}
	sess.mu.Unlock()
	st := m.SealStatus()
	if st.OK {
		t.Fatal("a box with a running turn says the seal would run")
	}
	if !strings.Contains(st.Reason, "running turn 2") {
		t.Errorf("the reason does not name the turn: %q", st.Reason)
	}
	if strings.HasPrefix(st.Reason, "busy:") {
		t.Errorf("the reason carries the error code: %q", st.Reason)
	}
	if !g.exists("work/project/main.go") {
		t.Fatal("asking whether a seal may run deleted the workspace")
	}

	// A preparation in progress is the other refusal.
	sess.mu.Lock()
	sess.busy = false
	sess.mu.Unlock()
	m.prep.mu.Lock()
	m.prep.st.State = PrepareRunning
	m.prep.st.Step = "clone"
	m.prep.mu.Unlock()
	st = m.SealStatus()
	if st.OK || !strings.Contains(st.Reason, "being prepared") {
		t.Fatalf("a box being prepared says the seal would run: %+v", st)
	}
}

// The seal turns a box into an image. Its test is a fake guest: the directories a
// seal cleans, laid out the way a box has them, so the test can assert on what is
// gone and what stayed.

const (
	// sealCredential is a made-up credential value. It is long enough for the
	// value search to look for it.
	sealCredential = "sk-test-9f8e7d6c5b4a3210"
	// sealRecipe is the recipe of the project whose box is being sealed.
	sealRecipe = "#!/bin/bash\nset -euo pipefail\napt-get install -y ripgrep\n"
)

// sealGuest is a box's guest tree with everything a seal touches.
type sealGuest struct {
	root string
	cfg  DaemonConfig
}

// newSealGuest lays out a guest with a workspace, a session, an executor home
// with a toolchain cache in it, a credential, a home with its copies and its
// history, a log, a journal directory, an ssh host key and a machine id.
func newSealGuest(t *testing.T) *sealGuest {
	t.Helper()
	root := t.TempDir()
	cfg := DaemonConfig{
		Socket:      filepath.Join(root, "boxd.sock"),
		StateDir:    filepath.Join(root, "var/lib/boxd"),
		RuntimeDir:  filepath.Join(root, "run/boxd"),
		SecretsDir:  filepath.Join(root, "run/box/secrets"),
		LocalDir:    filepath.Join(root, "etc/box/local"),
		PlatformDir: filepath.Join(root, "opt/box"),
		PrimeAgent:  buildFake(t),
		Journal:     io.Discard,
		Logf:        func(string, ...any) {},
		Fstrim:      "true",
		Journalctl:  "false",
		// A seal removes an account with userdel; the test's own script records
		// the call, so no account of the machine the test runs on is touched.
		Userdel: filepath.Join(root, "bin/userdel"),
	}
	g := &sealGuest{root: root, cfg: cfg}
	g.cfg.Seal = &SealPaths{
		Workspace: filepath.Join(root, "work"),
		Homes: []string{
			filepath.Join(root, "var/lib/boxd/homes/prime"),
			filepath.Join(root, "home/user"),
			filepath.Join(root, "home/debian.guest"),
			filepath.Join(root, "home/madarch"),
		},
		Temp:     []string{filepath.Join(root, "tmp"), filepath.Join(root, "var/tmp")},
		Logs:     []string{filepath.Join(root, "var/log")},
		HostKeys: filepath.Join(root, "etc/ssh"),
		CloudInit: []string{
			filepath.Join(root, "var/lib/cloud/instances"),
			filepath.Join(root, "var/lib/cloud/instance"),
			filepath.Join(root, "etc/machine-id"),
			filepath.Join(root, "var/lib/dbus/machine-id"),
		},
		Journal:      false,
		ImageRecord:  filepath.Join(root, "etc/box/image.json"),
		Recipe:       filepath.Join(root, "etc/box/recipe.sh"),
		MountInfo:    filepath.Join(root, "proc/self/mountinfo"),
		SystemdUnits: filepath.Join(root, "etc/systemd/system"),
		Fstab:        filepath.Join(root, "etc/fstab"),
		Sudoers:      filepath.Join(root, "etc/sudoers.d/90-cloud-init-users"),
		Passwd:       filepath.Join(root, "etc/passwd"),
		Linger:       filepath.Join(root, "var/lib/systemd/linger"),
	}
	return g
}

// write puts one file into the guest, creating its directory.
func (g *sealGuest) write(t *testing.T, rel, content string) string {
	t.Helper()
	path := filepath.Join(g.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mkdir creates a directory in the guest.
func (g *sealGuest) mkdir(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(g.root, rel)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// exists reports whether a guest path is there.
func (g *sealGuest) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(g.root, rel))
	return err == nil
}

// fill lays out the box this test seals: the work, the sessions, the caches, the
// credential and the state a seal has to get rid of.
func (g *sealGuest) fill(t *testing.T) {
	t.Helper()
	// The workspace: the project's sources.
	g.write(t, "work/project/main.go", "package main\n")
	g.write(t, "work/project/.git/config", "[remote]\n\turl = https://example.com/acme/project.git\n")
	// The box's last preparation, which recorded where its sources are.
	g.write(t, "var/lib/boxd/prepare.json", `{"state":"ready","step":"recipe","source":"git","dest":"`+
		filepath.Join(g.root, "work/project")+`"}`)
	// The sessions and their transcripts.
	g.write(t, "var/lib/boxd/state.json", `{"version":1,"sessions":[{"id":"s-1","executor":"prime"}]}`)
	g.write(t, "var/lib/boxd/sessions/s-1/events.jsonl", `{"seq":1,"kind":"turn_start"}`)
	g.write(t, "var/lib/boxd/sessions/s-1/prime/transcript.json", `{"prompt":"build the thing"}`)
	// The executor home: a toolchain cache, which stays, and its own state, which
	// does not.
	g.write(t, "var/lib/boxd/homes/prime/.cache/go-build/00/abc-d", "compiled object")
	g.write(t, "var/lib/boxd/homes/prime/go/pkg/mod/cache/download/x.zip", "module")
	g.write(t, "var/lib/boxd/homes/prime/.prime/agent/sessions/s-1.json", `{"prompt":"build"}`)
	g.write(t, "var/lib/boxd/homes/prime/.local/share/opencode/log/opencode.log", "log line")
	g.write(t, "var/lib/boxd/homes/prime/.bash_history", "git clone https://example.com/acme/project.git\n")
	// A configuration file of the box's own, which is not a credential.
	g.write(t, "var/lib/boxd/homes/prime/.config/tool/settings.ini", "jobs = 4\n")
	// The box's user: a home with a history, an ssh key, a stored git credential,
	// an npm token, a git credential helper and a copy of the credential file.
	g.write(t, "home/user/.bash_history", "curl https://example.com/acme/project.git\n")
	g.write(t, "home/user/.zsh_history", "make test\n")
	g.write(t, "home/user/.ssh/id_ed25519", "PRIVATE KEY\n")
	g.write(t, "home/user/.ssh/known_hosts", "example.com ssh-ed25519 AAAA\n")
	g.write(t, "home/user/.git-credentials", "https://user:secret@example.com\n")
	g.write(t, "home/user/.netrc", "machine example.com password secret\n")
	g.write(t, "home/user/.npmrc", "registry=https://registry.example.com/\n//registry.example.com/:_authToken="+sealCredential+"\n")
	g.write(t, "home/user/.gitconfig", "[user]\n\tname = A User\n[credential]\n\thelper = store\n[credential \"https://example.com\"]\n\thelper = cache\n")
	g.write(t, "home/user/notes.txt", "keep me\n")
	// The secrets directory is empty: the guest a seal runs in is a clone with
	// empty stand-ins (see TestSealRefusesAGuestWithCredentials), so this box has
	// no credential mounted.
	// boxd's own copies, derived from the secrets directory into the runtime
	// directory (a tmpfs) so an executor can write its settings: they are boxd's,
	// and a seal forgets them.
	g.write(t, "run/boxd/executors/prime/auth.json", `{"acme":{"type":"api_key","key":"`+sealCredential+`"}}`)
	g.write(t, "run/boxd/executors/prime/settings.json", `{"model":"acme/large"}`)
	g.write(t, "run/boxd/executors/prime/logs/executor.log", "log line\n")
	g.write(t, "run/boxd/executors/opencode-config/opencode.json", `{"model":"acme/large"}`)
	// The state a boot leaves behind.
	g.write(t, "tmp/build.log", "log\n")
	g.write(t, "var/tmp/scratch", "x\n")
	g.write(t, "var/log/syslog", "a boot happened\n")
	g.write(t, "var/log/journal/abc/system.journal", "journal\n")
	g.write(t, "etc/ssh/ssh_host_ed25519_key", "HOST KEY\n")
	g.write(t, "etc/ssh/sshd_config", "Port 22\n")
	g.write(t, "etc/machine-id", "0123456789abcdef0123456789abcdef\n")
	g.write(t, "var/lib/cloud/instances/i-1/obj.pkl", "cloud-init state\n")
	// The mount units Lima wrote for the instance the box was built in: the mounts
	// themselves are the node's, and their units must not travel in an image.
	g.write(t, "proc/self/mountinfo",
		"29 1 0:26 / / rw,relatime shared:1 - ext4 /dev/vda1 rw\n"+
			"30 29 0:41 / /etc/box/local ro,relatime shared:2 - 9p lima-w3a ro,cache=none,trans=virtio\n"+
			"31 29 0:42 / /opt/box ro,relatime shared:3 - 9p lima-w3a ro,cache=fscache,trans=virtio\n"+
			"32 29 0:43 / /run/box/secrets ro,relatime shared:4 - 9p lima-w3a ro,cache=none,trans=virtio\n")
	g.write(t, "etc/systemd/system/etc-box-local.mount",
		"[Unit]\nDescription=/etc/box/local\n\n[Mount]\nWhat=lima-w3a\nWhere=/etc/box/local\nType=9p\n")
	g.write(t, "etc/systemd/system/opt-box.mount",
		"[Unit]\nDescription=/opt/box\n\n[Mount]\nWhat=lima-w3a\nWhere=/opt/box\nType=9p\n")
	g.write(t, "etc/systemd/system/var-lib-boxd\x2ddata.mount", "[Mount]\nWhere=/var/lib/boxd/data\n")
	g.write(t, "etc/systemd/system/multi-user.target.wants/etc-box-local.mount", "")
	// The mount table of the instance the box was built in: its lines mount the
	// node's shares with that instance's tag, so a baked image must not carry them.
	g.write(t, "etc/fstab",
		"# /etc/fstab: the box's own root, and the node's mounts of this instance\n"+
			"LABEL=cloudimg-rootfs / ext4 defaults 0 0\n"+
			"lima-w3a /etc/box/local 9p ro,trans=virtio,cache=none,nofail 0 0\n"+
			"lima-w3a /run/box/secrets 9p ro,trans=virtio,cache=none,nofail 0 0\n"+
			"/swap.img none swap sw 0 0\n")
	// The platform bundle, mounted read-only at /opt/box: the seal runs its
	// guest/no-secrets.sh from it and never writes into it.
	g.mkdir(t, "opt/box/bin")
	g.writeNoSecrets(t, 0)
	// The login account Lima made on the box's first boot (uid 1000): its
	// account, its home, the other name Lima links that home under, its linger
	// entry, and cloud-init's record of it. cloud-init also left the rule of the
	// build account the image no longer has: a name the account table does not
	// have is not an account.
	g.write(t, "etc/passwd", "root:x:0:0:root:/root:/bin/bash\n"+
		"systemd-network:x:998:998:Systemd Network:/:/usr/sbin/nologin\n"+
		"debian:x:1000:1000:Debian:"+filepath.Join(g.root, "home/debian.guest")+":/bin/bash\n"+
		"madarch:x:1001:1001:Project:"+filepath.Join(g.root, "home/madarch")+":/bin/bash\n")
	g.write(t, "etc/sudoers.d/90-cloud-init-users",
		"# Created by cloud-init\n\n# User rules for builder\nbuilder ALL=(ALL) NOPASSWD:ALL\n\n"+
			"# User rules for debian\ndebian ALL=(ALL) NOPASSWD:ALL\n")
	g.write(t, "var/lib/systemd/linger/debian", "")
	g.write(t, "var/lib/systemd/linger/madarch", "")
	g.write(t, "home/debian.guest/.bash_history", "sudo apt-get install ripgrep\n")
	g.write(t, "home/debian.guest/.cache/tool/blob", "cache\n")
	// Lima links the account's home under the name it used before 2.1.
	if err := os.Symlink("debian.guest", filepath.Join(g.root, "home/debian.linux")); err != nil {
		t.Fatal(err)
	}
	// The project account a recipe created (uid 1001) and its toolchain cache:
	// the image keeps both.
	g.write(t, "home/madarch/go/pkg/mod/cache/download/x.zip", "module")
	g.write(t, "home/madarch/.config/tool/settings.ini", "jobs = 4\n")
	g.writeUserdel(t, 0)
}

// writeUserdel puts the test's userdel in the guest. It records the call and,
// on success, removes the account from the fake passwd table the way userdel
// does; code is the exit status the call reports.
func (g *sealGuest) writeUserdel(t *testing.T, code int) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + filepath.Join(g.root, "userdel.log") + "\"\n"
	if code == 0 {
		script += "sed -i \"/^$3:/d\" \"" + filepath.Join(g.root, "etc/passwd") + "\"\n"
	}
	script += fmt.Sprintf("exit %d\n", code)
	path := filepath.Join(g.root, "bin/userdel")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeNoSecrets puts a guest/no-secrets.sh into the fake bundle; the exit code
// is the one the check reports.
func (g *sealGuest) writeNoSecrets(t *testing.T, code int) {
	t.Helper()
	script := "#!/bin/bash\nsay() { printf '%s\\n' \"$*\"; }\nsay '== no credential in the image'\n"
	if code == 0 {
		script += "say 'ok    /root/.prime does not exist'\nsay ''\nsay 'RESULT: no credential in the image'\nexit 0\n"
	} else {
		script += "say 'FAIL  /var/lib/boxd/executors is not empty:'\nexit 1\n"
	}
	g.write(t, "opt/box/lib/no-secrets.sh", script)
	if err := os.Chmod(filepath.Join(g.root, "opt/box/lib/no-secrets.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// manager builds the daemon manager over the guest.
func (g *sealGuest) manager(t *testing.T) *Manager {
	t.Helper()
	return newManager(t, g.cfg)
}

// seal runs one seal and returns its result and error.
func seal(t *testing.T, m *Manager, req SealRequest) (SealResult, error) {
	t.Helper()
	var lines []string
	res, err := m.Seal(req, func(line string) { lines = append(lines, line) })
	res.Steps = append(res.Steps, lines...)
	return res, err
}

// sealRequest is the request of a project image named after its box.
func sealRequest() SealRequest {
	return SealRequest{
		Name:    "project",
		Version: "project-1",
		Arch:    "amd64",
		Parent:  "1.0.0",
		Recipe:  base64.StdEncoding.EncodeToString([]byte(sealRecipe)),
	}
}

// TestSealRemovesTheWorkAndKeepsTheCaches is the whole point of a seal: the code,
// the sessions and the credentials go, and the toolchains an image is for stay.
func TestSealRemovesTheWorkAndKeepsTheCaches(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)

	res, err := seal(t, m, sealRequest())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if res.Image.Name != "project" || res.Image.Version != "project-1" || res.Image.Parent != "1.0.0" {
		t.Fatalf("the image record is %+v", res.Image)
	}
	if res.Image.Platform != "mount" {
		t.Fatalf("the image must say it takes a platform bundle, got %q", res.Image.Platform)
	}

	for _, gone := range []string{
		"work",
		"var/lib/boxd/state.json",
		"var/lib/boxd/sessions",
		"var/lib/boxd/prepare.json",
		"var/lib/boxd/homes/prime/.prime",
		"var/lib/boxd/homes/prime/.local/share/opencode",
		"var/lib/boxd/homes/prime/.bash_history",
		"home/user/.bash_history",
		"home/user/.zsh_history",
		"home/user/.ssh/id_ed25519",
		"home/user/.git-credentials",
		"home/user/.netrc",
		"tmp/build.log",
		"var/tmp/scratch",
		"var/log/syslog",
		"var/log/journal",
		"etc/ssh/ssh_host_ed25519_key",
		"var/lib/cloud/instances/i-1",
		"etc/systemd/system/etc-box-local.mount",
		"etc/systemd/system/opt-box.mount",
		"etc/systemd/system/multi-user.target.wants/etc-box-local.mount",
		// boxd's own copies of the credentials, on the tmpfs: the seal forgets
		// them, and guest/no-secrets.sh asks a sealed box for them.
		"run/boxd/executors/prime/auth.json",
		"run/boxd/executors/prime/settings.json",
		"run/boxd/executors/opencode-config",
	} {
		if g.exists(gone) {
			t.Errorf("%s is still in the image", gone)
		}
	}
	for _, kept := range []string{
		"var/lib/boxd/homes/prime/.cache/go-build/00/abc-d",
		"etc/systemd/system/var-lib-boxd\x2ddata.mount",
		"var/lib/boxd/homes/prime/go/pkg/mod/cache/download/x.zip",
		"home/user/notes.txt",
		"etc/ssh/sshd_config",
		"opt/box/lib/no-secrets.sh",
	} {
		if !g.exists(kept) {
			t.Errorf("%s was deleted; an image keeps it", kept)
		}
	}
	// The mount table keeps the image's own mounts and loses the instance's.
	if data, err := os.ReadFile(filepath.Join(g.root, "etc/fstab")); err != nil ||
		strings.Contains(string(data), "/etc/box/local") || strings.Contains(string(data), "/run/box/secrets") ||
		!strings.Contains(string(data), "cloudimg-rootfs") || !strings.Contains(string(data), "/swap.img") {
		t.Errorf("the fstab is %q, %v", data, err)
	}
	// The machine id is emptied, not deleted: a distribution's boot generates it.
	if data, err := os.ReadFile(filepath.Join(g.root, "etc/machine-id")); err != nil || len(data) != 0 {
		t.Errorf("etc/machine-id: %q, %v", data, err)
	}
	// The npm token line goes, the registry stays.
	if data, err := os.ReadFile(filepath.Join(g.root, "home/user/.npmrc")); err != nil ||
		strings.Contains(string(data), sealCredential) || !strings.Contains(string(data), "registry.example.com") {
		t.Errorf("the .npmrc is %q, %v", data, err)
	}
	// The git credential helper goes; the user's own settings stay.
	if data, err := os.ReadFile(filepath.Join(g.root, "home/user/.gitconfig")); err != nil ||
		strings.Contains(string(data), "helper") || !strings.Contains(string(data), "A User") {
		t.Errorf("the .gitconfig is %q, %v", data, err)
	}
}

// TestSealWritesTheImageRecord checks what the image says about itself, and the
// recipe it carries for a rebuild on a newer parent image.
func TestSealWritesTheImageRecord(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)
	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(g.root, "etc/box/image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record ImageRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("the record is not JSON: %v", err)
	}
	if record.Schema != 1 || record.Name != "project" || record.Version != "project-1" {
		t.Fatalf("the record is %+v", record)
	}
	if record.Parent != "1.0.0" || record.Arch != "amd64" || record.Platform != "mount" {
		t.Fatalf("the record is %+v", record)
	}
	if record.SealedAt == "" {
		t.Fatal("the record carries no time")
	}
	recipe, err := base64.StdEncoding.DecodeString(record.Recipe)
	if err != nil || string(recipe) != sealRecipe {
		t.Fatalf("the record's recipe is %q, %v", recipe, err)
	}
	// The recipe is also a script in the guest, so a rebuild can run it there.
	script, err := os.ReadFile(filepath.Join(g.root, "etc/box/recipe.sh"))
	if err != nil || string(script) != sealRecipe {
		t.Fatalf("the recipe in the guest is %q, %v", script, err)
	}
	if info, err := os.Stat(filepath.Join(g.root, "etc/box/recipe.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the recipe is not executable: %v, %v", info, err)
	}
}

// TestSealDoesNotSearchTheDiskForTheCredential is the owner's decision: the seal
// no longer walks the disk looking for the credential values, so a file that
// merely mentions one does not stop it. guest/no-secrets.sh is the proof that
// stays, and it checks the places a credential is written into, not the text of
// every file.
func TestSealDoesNotSearchTheDiskForTheCredential(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	// A file whose name says nothing and whose text holds the value: the search
	// this seal used to run failed on exactly this.
	note := g.write(t, "home/user/src/notes/2026-02-01.txt", "the key was "+sealCredential+"\n")
	m := g.manager(t)

	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("the seal searched the disk for the credential and failed: %v", err)
	}
	// And it did not delete the file either: a seal deletes what it knows is
	// state, and this is the owner's own text.
	if !g.exists("home/user/src/notes/2026-02-01.txt") {
		t.Error("the seal deleted a file it only knew about from a value search")
	}
	if _, err := os.Stat(note); err != nil {
		t.Errorf("%s is gone: %v", note, err)
	}
}

// TestSealFailsWhenNoSecretsFails checks that the bundle's own check is a gate:
// the seal does not finish on a box that check rejects.
func TestSealFailsWhenNoSecretsFails(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	g.writeNoSecrets(t, 1)
	m := g.manager(t)

	_, err := seal(t, m, sealRequest())
	if err == nil {
		t.Fatal("the seal finished although guest/no-secrets.sh failed")
	}
	if !strings.Contains(err.Error(), "no-secrets.sh failed") {
		t.Errorf("the error does not name the check: %s", err)
	}
	if !strings.Contains(err.Error(), "/var/lib/boxd/executors is not empty") {
		t.Errorf("the error does not quote the failure: %s", err)
	}
	if !g.exists("home/user/notes.txt") {
		t.Error("a failed seal must leave the box as it is, but the home was emptied")
	}
}

// TestSealNeedsTheBundleCheck: an image is only an image if the check that proves
// it can run, so a bundle without the script is a seal that does not finish.
func TestSealNeedsTheBundleCheck(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	if err := os.Remove(filepath.Join(g.root, "opt/box/lib/no-secrets.sh")); err != nil {
		t.Fatal(err)
	}
	m := g.manager(t)
	_, err := seal(t, m, sealRequest())
	if err == nil || !strings.Contains(err.Error(), "lib/no-secrets.sh") {
		t.Fatalf("the seal did not refuse a bundle without the check: %v", err)
	}
}

// TestSealRefusesWhileATurnRuns: a seal deletes what a turn is working on.
func TestSealRefusesWhileATurnRuns(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)
	s := mustSession(t, m, "prime", "acme/model")
	sess := m.lookupSession(s.ID)
	sess.mu.Lock()
	sess.busy = true
	sess.cur = &turnState{Turn: Turn{Session: s.ID, Turn: 1, State: TurnRunning}, done: make(chan struct{})}
	sess.mu.Unlock()

	_, err := seal(t, m, sealRequest())
	if err == nil || CodeOf(err) != CodeBusy {
		t.Fatalf("the seal ran while a turn was running: %v", err)
	}
	if !strings.Contains(err.Error(), "running turn 1") {
		t.Errorf("the error does not name the turn: %s", err)
	}
	if !g.exists("work/project/main.go") {
		t.Error("the refused seal deleted the workspace anyway")
	}
}

// TestSealRefusesWhileAPreparationRuns: the sources of a preparation are exactly
// what the seal deletes, so the two cannot run at once.
func TestSealRefusesWhileAPreparationRuns(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)
	m.prep.mu.Lock()
	m.prep.st.State = PrepareRunning
	m.prep.st.Step = "clone"
	m.prep.st.LastLine = "Receiving objects:  40%"
	m.prep.mu.Unlock()

	_, err := seal(t, m, sealRequest())
	if err == nil || CodeOf(err) != CodeBusy {
		t.Fatalf("the seal ran during a preparation: %v", err)
	}
	if !strings.Contains(err.Error(), "being prepared") {
		t.Errorf("the error does not say what blocked it: %s", err)
	}
}

// TestSealForgetsTheSessions: a sealed daemon keeps no session, so the shutdown
// that follows a seal (the box is stopped) cannot write them back into the image.
func TestSealForgetsTheSessions(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)
	s := mustSession(t, m, "prime", "acme/model")

	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if n := len(m.List()); n != 0 {
		t.Fatalf("the manager still holds %d session(s)", n)
	}
	// Boxd's own shutdown runs after the seal, and any write of the state
	// directory at all: none of them may put back what the seal deleted.
	m.persist(nil)
	m.Shutdown()
	if g.exists("var/lib/boxd/state.json") {
		t.Error("the shutdown wrote state.json back after the seal")
	}
	if g.exists("var/lib/boxd/sessions") {
		t.Error("the shutdown recreated the session directory after the seal")
	}
	_, err := m.NewSession("prime", "acme/model", "", "after the seal")
	if err == nil || CodeOf(err) != CodeClosed {
		t.Fatalf("a sealed box started a session: %v, %s", err, s.ID)
	}
}

// TestSealRejectsAnUnusableName: the name and the version end up in a file name
// and in an image index, so a value that is not plain is refused before anything
// is deleted.
func TestSealRejectsAnUnusableName(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)
	for _, req := range []SealRequest{
		{Name: ""},
		{Name: "Project"},
		{Name: "project/../etc"},
		{Name: "project", Version: "1.0 0"},
		{Name: "project", Arch: "riscv64"},
	} {
		if _, err := seal(t, m, req); err == nil || CodeOf(err) != CodeInvalid {
			t.Errorf("seal %+v was accepted: %v", req, err)
		}
	}
	if !g.exists("work/project/main.go") {
		t.Error("a refused seal deleted the workspace")
	}
}

// TestZeroFreeSpaceStopsAtItsLimit: the fallback for a guest without fstrim, with
// a bound so that a test does not fill the machine it runs on.
func TestZeroFreeSpaceStopsAtItsLimit(t *testing.T) {
	g := newSealGuest(t)
	fillDir := t.TempDir()
	s := &sealer{cfg: g.cfg, paths: g.cfg.sealPaths(), say: func(string) {}, zeroLimit: 3 << 20, zeroPath: fillDir}
	if err := s.zeroFreeSpace(); err != nil {
		t.Fatalf("zeroFreeSpace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fillDir, "zero.fill")); !os.IsNotExist(err) {
		t.Fatalf("the zero file was left behind: %v", err)
	}
}

// TestClientSealStreamsTheSteps drives the daemon and its client over a real unix
// socket: a seal is a stream of progress lines, and the last one is the result.
func TestClientSealStreamsTheSteps(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	d, err := NewDaemon(g.cfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx) }()
	t.Cleanup(d.shutdown)
	waitForSocket(t, g.cfg.Socket)

	c := NewClient(g.cfg.Socket)
	var lines []string
	res, err := c.Seal(ctx, sealRequest(), func(line string) { lines = append(lines, line) })
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(lines) < 5 {
		t.Fatalf("the seal streamed %d line(s): %v", len(lines), lines)
	}
	if res.Image.Name != "project" || len(res.Steps) == 0 {
		t.Fatalf("the result is %+v", res)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "sessions") || !strings.Contains(joined, "no-secrets.sh") {
		t.Errorf("the progress lines do not say what was done:\n%s", joined)
	}
}

// waitForSocket waits until the daemon listens.
func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the daemon did not listen on %s", socket)
}

// TestSealRefusesAGuestWithCredentials: a seal makes an image, and an image must
// never be made of a machine that holds the box's credentials. `box bake` seals a
// clone whose secrets directory is an empty stand-in, so this is what keeps a
// seal out of a box somebody works in.
func TestSealRefusesAGuestWithCredentials(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	g.write(t, "run/box/secrets/prime/auth.json", `{"acme":{"type":"api_key","key":"`+sealCredential+`"}}`)
	m := g.manager(t)

	_, err := seal(t, m, sealRequest())
	if err == nil {
		t.Fatal("the seal ran in a guest that has the credentials mounted")
	}
	if !strings.Contains(err.Error(), "credentials mounted") {
		t.Errorf("the error does not say what stopped it: %s", err)
	}
	if !strings.Contains(err.Error(), "Bake the box instead") {
		t.Errorf("the error does not say what to do: %s", err)
	}
	if CodeOf(err) != CodeInvalid {
		t.Errorf("the refusal carries the code %s", CodeOf(err))
	}
	// Nothing was deleted: the refusal comes before the first removal.
	if !g.exists("work/project/main.go") {
		t.Error("the refused seal deleted the workspace")
	}
	// An empty secrets directory is what a bake's clone has: it is not a refusal.
	if err := os.Remove(filepath.Join(g.root, "run/box/secrets/prime/auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(g.root, "run/box/secrets/prime")); err != nil {
		t.Fatal(err)
	}
	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("the seal refused a guest whose secrets directory is empty: %v", err)
	}
}

// TestSealRemovesTheLoginAccountAndKeepsTheProjectAccount: the account Lima made
// on the box's first boot must not travel in an image -- the next node's user
// has another name and often the same uid, and cloud-init then fails with "UID
// 1000 is not unique" -- while the accounts a recipe created stay with their
// homes and caches.
func TestSealRemovesTheLoginAccountAndKeepsTheProjectAccount(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)

	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	calls, err := os.ReadFile(filepath.Join(g.root, "userdel.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(calls), "-f -r debian"); got != 1 {
		t.Errorf("the seal called userdel %d time(s) for the login account, want 1: %q", got, calls)
	}
	if strings.Contains(string(calls), "madarch") {
		t.Errorf("the seal removed the project account: %q", calls)
	}

	passwd, err := os.ReadFile(filepath.Join(g.root, "etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(passwd), "debian:") {
		t.Errorf("the login account is still in the account table: %q", passwd)
	}
	if !strings.Contains(string(passwd), "madarch:") {
		t.Errorf("the project account is gone from the account table: %q", passwd)
	}

	for _, gone := range []string{
		"var/lib/systemd/linger/debian",
		"home/debian.guest",
		"home/debian.linux",
	} {
		if g.exists(gone) {
			t.Errorf("%s is still in the image", gone)
		}
	}
	for _, kept := range []string{
		"var/lib/systemd/linger/madarch",
		"home/madarch/go/pkg/mod/cache/download/x.zip",
		"home/madarch/.config/tool/settings.ini",
	} {
		if !g.exists(kept) {
			t.Errorf("%s was deleted; an image keeps it", kept)
		}
	}
	// cloud-init's rule for the login account goes with it; the next first boot
	// writes the box's own user's rule again.
	sudoers := filepath.Join(g.root, "etc/sudoers.d/90-cloud-init-users")
	if data, err := os.ReadFile(sudoers); err == nil {
		if strings.Contains(string(data), "debian") || strings.Contains(string(data), "builder") {
			t.Errorf("cloud-init's sudoers file still names a login account: %q", data)
		}
	} else if !os.IsNotExist(err) {
		t.Error(err)
	}
}

// TestSealRemovesTheLoginAccountOnlyOnce: a seal runs again on a box it partly
// cleaned, so an account that is already gone is not a failure and does not run
// userdel again.
func TestSealRemovesTheLoginAccountOnlyOnce(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	m := g.manager(t)

	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("first Seal: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(g.root, "userdel.log"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("second Seal: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(g.root, "userdel.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Errorf("the second seal called userdel again: %q became %q", first, second)
	}
}

// TestSealFindsTheLoginAccountWithoutCloudInitRecord: cloud-init's sudoers file
// is the record a seal trusts first, but an image may arrive without one. The
// account then keeps the first regular uid a Linux node's user has.
func TestSealFindsTheLoginAccountWithoutCloudInitRecord(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	// A plain home: only the uid says this account is the login account.
	g.write(t, "etc/passwd", "root:x:0:0:root:/root:/bin/bash\n"+
		"debian:x:1000:1000:Debian:"+filepath.Join(g.root, "home/debian")+":/bin/bash\n"+
		"madarch:x:1001:1001:Project:"+filepath.Join(g.root, "home/madarch")+":/bin/bash\n")
	if err := os.Remove(filepath.Join(g.root, "etc/sudoers.d/90-cloud-init-users")); err != nil {
		t.Fatal(err)
	}
	m := g.manager(t)

	if _, err := seal(t, m, sealRequest()); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(g.root, "userdel.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "-f -r debian") {
		t.Errorf("the seal did not remove the uid 1000 account: %q", calls)
	}
	if strings.Contains(string(calls), "madarch") {
		t.Errorf("the seal removed the uid 1001 project account: %q", calls)
	}
	if !g.exists("home/madarch/.config/tool/settings.ini") {
		t.Error("the project account's home was deleted")
	}
}

// TestSealFailsWhenUserdelFails: an image that still has the login account
// cannot boot on the next node, so a userdel that fails stops the seal instead
// of leaving the account in place.
func TestSealFailsWhenUserdelFails(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	g.writeUserdel(t, 1)
	m := g.manager(t)

	_, err := seal(t, m, sealRequest())
	if err == nil {
		t.Fatal("the seal finished although userdel failed")
	}
	if !strings.Contains(err.Error(), "userdel -f -r debian") {
		t.Errorf("the error does not name the account and the command: %s", err)
	}
	if !strings.Contains(err.Error(), "the seal stopped at removing the login account Lima made") {
		t.Errorf("the error does not say which step failed: %s", err)
	}
}

// TestSealKeepsAProjectAccountWithTheLoginUID: a node whose user has another uid
// (a macOS user is 501) leaves uid 1000 free for the first account a recipe
// creates. A seal run again owns no cloud-init rule for that account and must
// not fall back to the uid and remove it.
func TestSealKeepsAProjectAccountWithTheLoginUID(t *testing.T) {
	g := newSealGuest(t)
	g.fill(t)
	g.write(t, "etc/passwd", "root:x:0:0:root:/root:/bin/bash\n"+
		"alice:x:501:501:Alice:"+filepath.Join(g.root, "home/alice.guest")+":/bin/bash\n"+
		"madarch:x:1000:1000:Project:"+filepath.Join(g.root, "home/madarch")+":/bin/bash\n")
	g.write(t, "etc/sudoers.d/90-cloud-init-users",
		"# Created by cloud-init\n\n# User rules for alice\nalice ALL=(ALL) NOPASSWD:ALL\n")
	g.write(t, "var/lib/systemd/linger/alice", "")
	g.write(t, "home/alice.guest/.bashrc", "# alice\n")
	m := g.manager(t)

	for run := 1; run <= 2; run++ {
		if _, err := seal(t, m, sealRequest()); err != nil {
			t.Fatalf("seal %d: %v", run, err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(g.root, "userdel.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "madarch") {
		t.Errorf("the seal removed the project account with uid 1000: %q", calls)
	}
	passwd, err := os.ReadFile(filepath.Join(g.root, "etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(passwd), "madarch:") {
		t.Errorf("the project account with uid 1000 is gone: %q", passwd)
	}
	if !g.exists("home/madarch/.config/tool/settings.ini") {
		t.Error("the project account's home was deleted")
	}
}
