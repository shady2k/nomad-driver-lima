//go:build integration

// The root-mode scenarios: a Nomad client that runs as root, which is what a
// Linux node does, with the box driver dropping every Lima process to an
// unprivileged runtime user.
//
// scripts/integration.sh --root starts the agents as root and runs this test
// alone (the full suite resizes a box to 4 vCPU and kills the client as the test
// user, neither of which belongs here).

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rootFileTemplates are rendered into the task's local/ and secrets/ directories
// by Nomad, which runs as root: the driver has to stage them into a directory the
// runtime user can read before limactl copy can reach them. The plain suite
// renders its own files (taskFileTemplates); this one adds a file under each root
// so the root scenario can check both destinations and their owners.
const rootFileTemplates = `
      template {
        destination = "local/root-local.txt"
        data        = "root-check-local"
      }

      template {
        destination = "secrets/root-check.txt"
        data        = "root-check-secrets"
      }
`

// renderRootJob renders the example job with one local and one secrets file.
func (e *testEnv) renderRootJob(t *testing.T) string {
	t.Helper()
	base := e.renderJob(t, defaultJobParams())
	data, err := os.ReadFile(base)
	require.NoError(t, err)
	rendered := strings.Replace(string(data), "\n      volume_mount {", rootFileTemplates+"\n      volume_mount {", 1)
	require.Contains(t, rendered, "local/root-local.txt")
	path := filepath.Join(t.TempDir(), "box-root.nomad.hcl")
	require.NoError(t, os.WriteFile(path, []byte(rendered), 0o644))
	return path
}

// uidOf returns the uid of a local user.
func uidOf(t *testing.T, name string) int {
	t.Helper()
	u, err := user.Lookup(name)
	require.NoError(t, err, "the runtime user %q must exist", name)
	uid, err := strconv.Atoi(u.Uid)
	require.NoError(t, err)
	return uid
}

// readPID reads a pid file the harness writes.
func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.NotZero(t, pid)
	return pid
}

// procUID returns the real uid a process runs as. /proc/<pid>/status is readable
// for every user, so the test does not need root for this.
func procUID(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	require.NoError(t, err, "read the status of pid %d", pid)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		require.NotEmpty(t, fields)
		uid, err := strconv.Atoi(fields[0])
		require.NoError(t, err)
		return uid
	}
	require.FailNow(t, "no Uid line in /proc status", "pid %d", pid)
	return -1
}

// fileUID returns the owner of a path, without following a symlink.
func fileUID(t *testing.T, path string) int {
	t.Helper()
	st, err := os.Lstat(path)
	require.NoError(t, err)
	raw, ok := st.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return int(raw.Uid)
}

// readLink returns the target of a symlink.
func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	require.NoError(t, err)
	return target
}

// inode returns the inode of a file, which identifies the disk across a stop and
// a start.
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	raw, ok := st.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return raw.Ino
}

// requireOwnedBy walks root and fails with every entry that does not belong to
// uid. Everything Lima touches has to belong to the runtime user, or the next
// limactl call fails with a permission error.
func requireOwnedBy(t *testing.T, root string, uid int) {
	t.Helper()
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		raw, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if int(raw.Uid) != uid {
			offenders = append(offenders, fmt.Sprintf("%s (uid %d)", path, raw.Uid))
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(offenders)
	require.Empty(t, offenders, "these entries below %s do not belong to the runtime user", root)
}

// restartClient kills the root Nomad client and starts it again through the
// harness script (which uses sudo itself).
func (e *testEnv) restartClient(t *testing.T) {
	t.Helper()
	pid := readPID(t, e.clientPID)
	out, err := exec.Command("sudo", "-n", "kill", "-9", strconv.Itoa(pid)).CombinedOutput()
	require.NoError(t, err, "kill the root Nomad client: %s", out)
	time.Sleep(3 * time.Second)
	cmd := exec.Command(e.startClient)
	require.NoError(t, cmd.Start(), "start the Nomad client again")
	go func() { _ = cmd.Wait() }()
}

// waitExec waits until a command can be run in the guest again. After a client
// restart the allocation is reported running before the task is, so the driver
// and the plugin have to be back first.
func (e *testEnv) waitExec(t *testing.T, alloc string, timeout time.Duration) {
	t.Helper()
	var last string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := e.nomadTry("alloc", "exec", "-task", taskName, alloc, "/bin/sh", "-c", "true")
		last = out
		if err == nil && !strings.Contains(out, "not running") {
			return
		}
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "the task never became runnable again", "last output: %s", last)
}

// nodeAttrs waits for the node attributes of the ready node.
func (e *testEnv) nodeAttrs(t *testing.T) map[string]string {
	t.Helper()
	var attrs map[string]string
	e.waitFor(t, "the node attributes", 3*time.Minute, func() bool {
		found, err := e.nodeAttrTry()
		if err != nil {
			return false
		}
		attrs = found
		return true
	})
	return attrs
}

func TestBoxRootLifecycle(t *testing.T) {
	if os.Getenv("BOX_TEST_ROOT_MODE") != "1" {
		t.Skip("the root scenarios need scripts/integration.sh --root")
	}
	e := loadEnv(t)
	runtimeUID := uidOf(t, e.runtimeUser)
	require.NotZero(t, runtimeUID, "the runtime user must not be root")

	var (
		allocID string
		jobFile = e.renderRootJob(t)
		volDir  string
		disk    string
		marker  = "root-scenario-1"
	)

	t.Run("1 create: the root client runs the box as the runtime user", func(t *testing.T) {
		// The point of the scenario: the client is root. The client creates its
		// own alloc directory, so its owner says who runs the client.
		require.Equal(t, 0, fileUID(t, filepath.Join(e.work, "data", "client", "alloc")),
			"this scenario needs the Nomad client to run as root")

		e.nomad(t, "volume", "create", e.renderVolume(t))
		require.Contains(t, e.nomad(t, "volume", "status", "-type=host", volumeID), "ready")

		e.nomad(t, "job", "run", "-detach", jobFile)
		allocID = e.waitRunningAlloc(t, 8*time.Minute)

		// The driver reports the user it drops to.
		attrs := e.nodeAttrs(t)
		require.Equal(t, e.runtimeUser, attrs["driver.box.runtime_user"],
			"the fingerprint must name the runtime user")

		inst := e.instance(t)
		require.NotNil(t, inst, "the driver must create a lima instance named %s", boxID)
		require.Equal(t, "Running", inst.Status)
		require.Equal(t, runtimeUID, procUID(t, inst.DriverPID), "qemu must run as the runtime user")
		require.Equal(t, runtimeUID, procUID(t, inst.HostAgentPID), "the lima hostagent must run as the runtime user")

		// Everything Lima touches belongs to the runtime user.
		volDir = e.volumeHostPath(t)
		disk = filepath.Join(volDir, "box-"+boxID+".qcow2")
		require.FileExists(t, disk)
		require.Equal(t, runtimeUID, fileUID(t, volDir), "the volume directory")
		require.Equal(t, runtimeUID, fileUID(t, disk), "the box disk")
		require.Equal(t, runtimeUID, fileUID(t, inst.Dir), "the lima instance directory")
		link := filepath.Join(e.limaHome, boxID, "disk")
		require.Equal(t, disk, readLink(t, link))
		require.Equal(t, runtimeUID, fileUID(t, link), "the disk symlink")
		requireOwnedBy(t, e.limaHome, runtimeUID)
		requireOwnedBy(t, filepath.Join(e.work, "images", "cache"), runtimeUID)

		// The guest log stream and exec go through limactl as that user too.
		e.waitLogs(t, allocID, "guest-log-line", 4*time.Minute)
		require.Equal(t, "x86_64", e.guestSh(t, allocID, "uname -m"))

		// The task files Nomad rendered as root reach the guest through the
		// staging directory the runtime user owns.
		require.Equal(t, "root-check-local",
			e.guestSh(t, allocID, "sudo -n cat /etc/box/local/root-local.txt"))
		// The credentials are on the guest's tmpfs, never on the box disk.
		require.Equal(t, "root-check-secrets",
			e.guestSh(t, allocID, "sudo -n cat /run/box/secrets/root-check.txt"))
		require.Equal(t, "tmpfs", e.guestSh(t, allocID, "findmnt -no FSTYPE --target /run/box/secrets"))
		require.Equal(t, "700", e.guestSh(t, allocID, "sudo -n stat -c %a /run/box/secrets"))
		require.Equal(t, "root", e.guestSh(t, allocID, "sudo -n stat -c %U /run/box/secrets"))
		require.Empty(t, e.guestSh(t, allocID, "sudo -n find / -xdev -name root-check.txt -path '*secrets*' 2>/dev/null"),
			"a credential must not be on the box disk")
		// The staging directory is removed again.
		entries, err := os.ReadDir(filepath.Join(e.limaHome, "_staging"))
		require.NoError(t, err)
		require.Empty(t, entries, "the staging directory must be empty after a start")

		// The memory cap is a user scope of the runtime user.
		cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", inst.DriverPID))
		require.NoError(t, err)
		require.Contains(t, string(cgroup), "box-"+boxID+".scope",
			"qemu must run in the box scope of the runtime user: %s", cgroup)
		rel := strings.TrimSpace(strings.SplitN(string(cgroup), "::", 2)[1])
		memoryMax, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", rel, "memory.max"))
		require.NoError(t, err)
		wantBytes := int64(1024+150) * 1024 * 1024
		require.Equal(t, strconv.FormatInt(wantBytes, 10), strings.TrimSpace(string(memoryMax)))

		writeMarker(t, e, allocID, marker)
		require.Equal(t, marker, readMarker(t, e, allocID))
	})

	t.Run("2 a client restart keeps the allocation, the VM and its disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		pidBefore := e.instance(t).DriverPID
		e.restartClient(t)
		e.waitFor(t, "the node to reconnect", 3*time.Minute, func() bool {
			_, err := e.nodeAttrTry()
			return err == nil
		})
		e.waitFor(t, "the allocation to be restored", 3*time.Minute, func() bool {
			return e.allocRunning(t, allocID)
		})
		e.waitExec(t, allocID, 3*time.Minute)
		inst := e.instance(t)
		require.NotNil(t, inst)
		require.Equal(t, pidBefore, inst.DriverPID, "a client restart must not restart the VM")
		require.Equal(t, runtimeUID, procUID(t, inst.DriverPID))
		require.Equal(t, marker, readMarker(t, e, allocID))
	})

	t.Run("3 stop and start again keeps the disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		before := inode(t, disk)
		e.nomad(t, "job", "stop", "-detach", jobName)
		e.waitFor(t, "the VM to stop", 3*time.Minute, func() bool {
			inst := e.instance(t)
			return inst != nil && inst.Status == "Stopped"
		})
		require.FileExists(t, disk, "a stopped box keeps its disk")
		e.waitFor(t, "the old allocation to stop", 3*time.Minute, func() bool {
			for _, a := range e.allocs(t) {
				if a.ClientStatus == "running" || a.ClientStatus == "pending" {
					return false
				}
			}
			return true
		})

		e.nomad(t, "job", "run", "-detach", jobFile)
		next := e.waitRunningAlloc(t, 8*time.Minute, allocID)
		require.NotEqual(t, allocID, next, "a job run after a stop creates a new allocation")
		allocID = next
		require.Equal(t, marker, readMarker(t, e, allocID), "the disk must survive a stop and a start")
		require.Equal(t, before, inode(t, disk), "the same disk file must be reused")
		requireOwnedBy(t, e.limaHome, runtimeUID)
	})

	t.Run("4 rm deletes the job, the instance and the disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		e.nomad(t, "job", "stop", "-purge", jobName)
		e.waitFor(t, "the VM to stop", 3*time.Minute, func() bool {
			inst := e.instance(t)
			return inst == nil || inst.Status != "Running"
		})
		e.nomad(t, "volume", "delete", "-type=host", volumeID)
		e.waitFor(t, "the lima instance to be deleted", 2*time.Minute, func() bool {
			return e.instance(t) == nil
		})
		_, err := os.Stat(volDir)
		require.True(t, os.IsNotExist(err), "the volume directory must be gone, got %v", err)
	})
}
