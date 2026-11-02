//go:build integration_cli

// Package integration holds the CLI integration suite.
//
// These tests drive the real `box` binary against a real Nomad server and
// client and real Lima VMs; scripts/integration-cli.sh starts the cluster,
// builds the binary and the plugins, and sets the environment below.
//
// The suite runs one box VM of 1 vCPU / 1024 MiB (the host is busy) and deletes
// it at the end.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testBox = "it1"
	// testBox2 is the box of the reconciliation scenarios: it is created through
	// a proxy that drops the answer to the job registration, then removed and
	// created again with the same id.
	testBox2  = "it2"
	imageVers = "1.0.0"
	diskGiB   = 8

	// faultProxyPort is the proxy of scenario 14. It is inside the port range
	// this suite owns (24646-24658) so it never meets another harness.
	faultProxyPort = 24655
)

// indexSrv serves the image index and the image itself, so `box create` goes
// through its real HTTP code path.
var indexSrv *httptest.Server

func TestMain(m *testing.M) {
	image := os.Getenv("BOX_TEST_IMAGE")
	if image == "" {
		fmt.Fprintln(os.Stderr, "BOX_TEST_IMAGE is not set; run scripts/integration-cli.sh")
		os.Exit(1)
	}
	sum, size, err := fileDigest(image)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash image: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	index := map[string]any{
		"schema": 1,
		"images": map[string]any{
			imageVers: map[string]any{
				"amd64": map[string]any{
					"url":      "", // filled in below, once the server URL is known
					"sha256":   sum,
					"size":     size,
					"built_at": time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}
	mux.Handle("/images/", http.StripPrefix("/images/", http.FileServer(http.Dir(filepath.Dir(image)))))
	mux.HandleFunc("/box-base/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(index)
	})
	indexSrv = httptest.NewServer(mux)
	defer indexSrv.Close()

	artifact := index["images"].(map[string]any)[imageVers].(map[string]any)["amd64"].(map[string]any)
	artifact["url"] = indexSrv.URL + "/images/" + filepath.Base(image)

	os.Exit(m.Run())
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// cliEnv is the cluster the suite talks to.
type cliEnv struct {
	bin      string
	addr     string
	work     string
	limactl  string
	limaHome string
	image    string
	// home is the guest home directory, found once by the first test that
	// needs it.
	home string
}

func loadEnv(t *testing.T) *cliEnv {
	t.Helper()
	e := &cliEnv{
		bin:      os.Getenv("BOX_CLI"),
		addr:     os.Getenv("BOX_TEST_NOMAD_ADDR"),
		work:     os.Getenv("BOX_TEST_WORK"),
		limactl:  os.Getenv("BOX_TEST_LIMACTL"),
		limaHome: os.Getenv("BOX_TEST_LIMA_HOME"),
		image:    os.Getenv("BOX_TEST_IMAGE"),
	}
	for name, value := range map[string]string{
		"BOX_CLI":             e.bin,
		"BOX_TEST_NOMAD_ADDR": e.addr,
		"BOX_TEST_WORK":       e.work,
		"BOX_TEST_LIMACTL":    e.limactl,
		"BOX_TEST_LIMA_HOME":  e.limaHome,
		"BOX_TEST_IMAGE":      e.image,
	} {
		require.NotEmpty(t, value, "%s must be set (run scripts/integration-cli.sh)", name)
	}
	return e
}

// env is the environment of every box call: exactly what a coordinator sets.
func (e *cliEnv) env() []string { return e.envFor(e.addr) }

// envFor is env with another Nomad address, which is what the fault proxy of
// scenario 14 needs. An inherited NOMAD_ADDR is dropped: the first match in the
// environment wins, so appending would not override it.
func (e *cliEnv) envFor(addr string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NOMAD_ADDR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"NOMAD_ADDR="+addr,
		"BOX_IMAGE_INDEX="+indexSrv.URL+"/box-base/index.json",
		"BOX_LIMACTL="+e.limactl,
		"BOX_LIMA_HOME="+e.limaHome,
	)
}

// result is one CLI run.
type result struct {
	stdout string
	stderr string
	code   int
}

// box runs the CLI and returns its output and exit code.
func (e *cliEnv) box(t *testing.T, args ...string) result {
	t.Helper()
	return e.boxAt(t, e.addr, args...)
}

// boxAt runs the CLI against another Nomad address.
func (e *cliEnv) boxAt(t *testing.T, addr string, args ...string) result {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.envFor(addr)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("box %s did not run: %v", strings.Join(args, " "), err)
		}
	}
	t.Logf("$ box %s -> %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// guestHome returns the home directory of the user the guest commands run as.
//
// Guest calls are `limactl shell` of the box, which is an unprivileged user, so
// the tests write below $HOME and use sudo -n for system paths.
func (e *cliEnv) guestHome(t *testing.T) string {
	t.Helper()
	if e.home != "" {
		return e.home
	}
	res := e.boxOK(t, "run", testBox, "--", "sh", "-c", `printf %s "$HOME"`)
	home := strings.TrimSpace(res.stdout)
	require.True(t, strings.HasPrefix(home, "/"), "unexpected guest home %q", home)
	e.home = home
	return home
}

// installInGuest copies a local file into a system path of the guest.
func (e *cliEnv) installInGuest(t *testing.T, localPath, guestPath string) {
	t.Helper()
	home := e.guestHome(t)
	staged := home + "/staged-" + filepath.Base(localPath)
	e.boxOK(t, "cp", localPath, testBox+":"+staged)
	res := e.box(t, "run", testBox, "--", "sudo", "-n", "install", "-m", "0755", staged, guestPath)
	require.Equal(t, 0, res.code, "cannot install %s in the guest: %s%s", guestPath, res.stdout, res.stderr)
}

// clientLogTail returns the last lines of the Nomad client log, for a failure
// message that says what the driver did.
func (e *cliEnv) clientLogTail() string {
	data, err := os.ReadFile(filepath.Join(e.work, "client.log"))
	if err != nil {
		return "(no client log: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, " | ")
}

// nomad runs the nomad CLI against this cluster.
func (e *cliEnv) nomad(t *testing.T, args ...string) string {
	t.Helper()
	bin := os.Getenv("BOX_TEST_NOMAD")
	require.NotEmpty(t, bin, "BOX_TEST_NOMAD must be set (run scripts/integration-cli.sh)")
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "NOMAD_ADDR="+e.addr)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "nomad %s failed: %s", strings.Join(args, " "), out)
	return string(out)
}

// waitForNode waits until the cluster reports a ready node, using the CLI
// itself: `box capacity` is the coordinator's own view of the cluster.
func waitForNode(t *testing.T, e *cliEnv) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var last result
	for time.Now().Before(deadline) {
		last = e.box(t, "capacity", "--json")
		if last.code == 0 {
			var doc struct {
				Nodes []struct {
					Status string `json:"status"`
				} `json:"nodes"`
			}
			if json.Unmarshal([]byte(last.stdout), &doc) == nil && len(doc.Nodes) > 0 && doc.Nodes[0].Status == "ready" {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("no ready Nomad node: %s%s", last.stdout, last.stderr)
}

// volumeNames lists the dynamic host volumes Nomad reports.
func volumeNames(t *testing.T, e *cliEnv) []string {
	t.Helper()
	resp, err := http.Get(e.addr + "/v1/volumes?type=host")
	require.NoError(t, err)
	defer resp.Body.Close()
	var stubs []struct {
		Name string `json:"Name"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&stubs), "decode the host volumes")
	names := make([]string, 0, len(stubs))
	for _, stub := range stubs {
		names = append(names, stub.Name)
	}
	return names
}

func asExitError(err error, target **exec.ExitError) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		*target = exitErr
		return true
	}
	return false
}

// boxOK runs the CLI and requires exit code 0.
func (e *cliEnv) boxOK(t *testing.T, args ...string) result {
	t.Helper()
	res := e.box(t, args...)
	require.Equal(t, 0, res.code, "box %s failed: %s%s", strings.Join(args, " "), res.stdout, res.stderr)
	return res
}

// boxJSON runs the CLI with --json and decodes the document into out.
func (e *cliEnv) boxJSON(t *testing.T, out any, args ...string) {
	t.Helper()
	full := append([]string{}, args...)
	full = append(full, "--json")
	res := e.boxOK(t, full...)
	require.NoError(t, json.Unmarshal([]byte(res.stdout), out), "decode: %s", res.stdout)
}

func TestCLI(t *testing.T) {
	e := loadEnv(t)
	waitForNode(t, e)

	t.Run("1 capacity lists a node that can run boxes", func(t *testing.T) {
		var doc struct {
			Nodes []struct {
				Name      string                      `json:"name"`
				Arch      string                      `json:"arch"`
				OS        string                      `json:"os"`
				Status    string                      `json:"status"`
				Eligible  bool                        `json:"eligible"`
				MemoryMiB struct{ Total, Free int64 } `json:"memory_mib"`
				Driver    struct {
					Installed bool   `json:"installed"`
					Version   string `json:"version"`
					LimaVer   string `json:"lima_version"`
				} `json:"driver"`
				VolumePlugins []struct {
					ID      string `json:"id"`
					Version string `json:"version"`
				} `json:"volume_plugins"`
				Boxes int `json:"boxes"`
			} `json:"nodes"`
			Arches []struct {
				Arch      string `json:"arch"`
				Nodes     int    `json:"nodes"`
				MemoryMiB int64  `json:"memory_mib_free"`
			} `json:"arches"`
		}
		e.boxJSON(t, &doc, "capacity")
		require.Len(t, doc.Nodes, 1, "the harness starts one client")
		node := doc.Nodes[0]
		require.Equal(t, "amd64", node.Arch)
		require.Equal(t, "ready", node.Status)
		require.True(t, node.Eligible)
		require.True(t, node.Driver.Installed)
		require.NotEmpty(t, node.Driver.LimaVer)
		require.NotEmpty(t, node.VolumePlugins)
		require.Equal(t, "box-volume", node.VolumePlugins[0].ID)
		require.Greater(t, node.MemoryMiB.Free, int64(1500))
		require.Len(t, doc.Arches, 1)
		require.Equal(t, "amd64", doc.Arches[0].Arch)

		// The human table says the same thing.
		res := e.boxOK(t, "capacity")
		require.Contains(t, res.stdout, node.Name)
		require.Contains(t, res.stdout, node.VolumePlugins[0].Version)
	})

	t.Run("2 images lists the published base image", func(t *testing.T) {
		var doc struct {
			Available bool `json:"available"`
			Images    []struct {
				Version string `json:"version"`
				Arch    string `json:"arch"`
				URL     string `json:"url"`
				SHA256  string `json:"sha256"`
				Size    int64  `json:"size"`
			} `json:"images"`
		}
		e.boxJSON(t, &doc, "images")
		require.True(t, doc.Available)
		require.Len(t, doc.Images, 1)
		require.Equal(t, imageVers, doc.Images[0].Version)
		require.Equal(t, "amd64", doc.Images[0].Arch)
		require.Len(t, doc.Images[0].SHA256, 64)
		require.Greater(t, doc.Images[0].Size, int64(0))
	})

	t.Run("3 create makes the volume and the job and waits for the box", func(t *testing.T) {
		var created struct {
			Box struct {
				ID     string `json:"id"`
				State  string `json:"state"`
				Node   string `json:"node"`
				Arch   string `json:"arch"`
				CPUs   int    `json:"cpus"`
				Memory int    `json:"memory_mib"`
				Disk   int    `json:"disk_gib"`
				Alloc  string `json:"allocation_id"`
			} `json:"box"`
			Job    string `json:"job"`
			Volume string `json:"volume"`
			Node   string `json:"node"`
		}
		e.boxJSON(t, &created, "create", testBox, "--image", imageVers,
			"--cpus", "1", "--memory-mib", "1024", "--disk-gib", fmt.Sprint(diskGiB), "--timeout", "10m")
		require.Equal(t, testBox, created.Box.ID)
		require.Equal(t, "running", created.Box.State)
		require.Equal(t, fmt.Sprint("box-", testBox), created.Job)
		require.NotEmpty(t, created.Box.Alloc)
		require.Equal(t, 1, created.Box.CPUs)
		require.Equal(t, 1024, created.Box.Memory)
		require.Equal(t, diskGiB, created.Box.Disk)
		require.NotEmpty(t, created.Volume)

		// Creating the same id again is refused instead of replacing the disk.
		res := e.box(t, "create", testBox, "--image", imageVers)
		require.Equal(t, 1, res.code)
		require.Contains(t, res.stderr, "already exists")
	})

	t.Run("4 ls lists the box and the disk it occupies", func(t *testing.T) {
		type lsDoc struct {
			Boxes []struct {
				ID               string `json:"id"`
				Node             string `json:"node"`
				Arch             string `json:"arch"`
				State            string `json:"state"`
				Alloc            string `json:"allocation_id"`
				Volume           int64  `json:"volume_capacity_bytes"`
				DiskUsedBytes    int64  `json:"disk_used_bytes"`
				DiskVirtualBytes int64  `json:"disk_virtual_bytes"`
			} `json:"boxes"`
		}
		var doc lsDoc
		e.boxJSON(t, &doc, "ls")
		require.Len(t, doc.Boxes, 1)
		box := doc.Boxes[0]
		require.Equal(t, testBox, box.ID)
		require.Equal(t, "running", box.State)
		require.Equal(t, "amd64", box.Arch)
		require.NotEmpty(t, box.Node)
		require.NotEmpty(t, box.Alloc)
		require.Equal(t, int64(diskGiB)<<30, box.Volume, "Nomad reports the volume capacity")

		// The disk the VM really occupies reaches Nomad through the driver's
		// node attributes, and `box ls` reads it from there.
		require.Eventually(t, func() bool {
			var fresh lsDoc
			e.boxJSON(t, &fresh, "ls")
			return len(fresh.Boxes) == 1 &&
				fresh.Boxes[0].DiskUsedBytes > 0 &&
				fresh.Boxes[0].DiskVirtualBytes == int64(diskGiB)<<30
		}, 90*time.Second, 3*time.Second, "the disk usage never reached `box ls` (the driver fingerprints every 30s)")

		var capacity struct {
			Nodes []struct {
				NodeID string `json:"node_id"`
				Disks  []struct {
					BoxID        string `json:"box_id"`
					UsedBytes    int64  `json:"used_bytes"`
					VirtualBytes int64  `json:"virtual_bytes"`
				} `json:"box_disks"`
				DiskUsedBytes    int64 `json:"disk_used_bytes"`
				DiskVirtualBytes int64 `json:"disk_virtual_bytes"`
			} `json:"nodes"`
		}
		e.boxJSON(t, &capacity, "capacity")
		require.Len(t, capacity.Nodes, 1)
		nodeID := capacity.Nodes[0].NodeID

		// The owner asked for the disk a VM occupies to be visible in Nomad, so
		// it has to be in the node's own view, not only in the CLI.
		require.Eventually(t, func() bool {
			out := e.nomad(t, "node", "status", "-verbose", nodeID)
			return strings.Contains(out, "driver.box.disk."+testBox+".used_bytes") &&
				strings.Contains(out, "driver.box.disk.used_bytes") &&
				strings.Contains(out, "driver.box.disk."+testBox+".virtual_bytes")
		}, 90*time.Second, 3*time.Second, "the disk usage is not in the node attributes")

		res := e.boxOK(t, "ls")
		require.Contains(t, res.stdout, testBox)

		e.boxJSON(t, &capacity, "capacity")
		require.Len(t, capacity.Nodes, 1)
		require.Greater(t, capacity.Nodes[0].DiskUsedBytes, int64(0))
		require.Equal(t, int64(diskGiB)<<30, capacity.Nodes[0].DiskVirtualBytes)
		require.Len(t, capacity.Nodes[0].Disks, 1)
		require.Equal(t, testBox, capacity.Nodes[0].Disks[0].BoxID)
	})

	t.Run("5 status shows the job, the allocation and the volume", func(t *testing.T) {
		var doc struct {
			Box struct {
				ID               string `json:"id"`
				State            string `json:"state"`
				DiskUsedBytes    int64  `json:"disk_used_bytes"`
				DiskVirtualBytes int64  `json:"disk_virtual_bytes"`
			} `json:"box"`
			Spec struct {
				Image      string `json:"image"`
				Arch       string `json:"arch"`
				CPUs       int    `json:"cpus"`
				MemoryMiB  int    `json:"memory_mib"`
				TaskMemMiB int    `json:"task_memory_mib"`
			} `json:"spec"`
			Allocation *struct {
				ClientStatus string `json:"client_status"`
				TaskState    string `json:"task_state"`
			} `json:"allocation"`
			Volume *struct {
				Name          string `json:"name"`
				CapacityBytes int64  `json:"capacity_bytes"`
			} `json:"volume"`
			Guest      any    `json:"guest"`
			GuestError string `json:"guest_error"`
		}
		e.boxJSON(t, &doc, "status", testBox)
		require.Equal(t, "running", doc.Box.State)
		require.Equal(t, imageVers, doc.Spec.Image)
		require.Equal(t, "amd64", doc.Spec.Arch)
		require.Equal(t, 1024+150, doc.Spec.TaskMemMiB)
		require.NotNil(t, doc.Allocation)
		require.Equal(t, "running", doc.Allocation.ClientStatus)
		require.NotNil(t, doc.Volume)
		require.Equal(t, "box-"+testBox, doc.Volume.Name)
		require.Equal(t, int64(diskGiB)<<30, doc.Volume.CapacityBytes)
		require.Greater(t, doc.Box.DiskUsedBytes, int64(0), "`box status` says what the disk occupies")
		require.Equal(t, int64(diskGiB)<<30, doc.Box.DiskVirtualBytes)
		// The image has no boxd yet; either answer is fine, a crash is not.
		require.True(t, doc.Guest != nil || doc.GuestError != "", "the guest half must be reported")
	})

	t.Run("6 run returns the guest output and exit code", func(t *testing.T) {
		res := e.boxOK(t, "run", testBox, "--", "nproc")
		require.Equal(t, "1", strings.TrimSpace(res.stdout))

		res = e.box(t, "run", testBox, "--", "sh", "-c", "echo hello from the guest; exit 3")
		require.Equal(t, 3, res.code, "the guest exit code is the command's exit code")
		require.Contains(t, res.stdout, "hello from the guest")

		res = e.box(t, "run", testBox, "--", "sh", "-c", "echo to-stderr >&2")
		require.Contains(t, res.stderr, "to-stderr")
	})

	t.Run("7 cp copies a directory in and out", func(t *testing.T) {
		local := t.TempDir()
		tree := filepath.Join(local, "work")
		require.NoError(t, os.MkdirAll(filepath.Join(tree, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tree, "a.txt"), []byte("alpha\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("beta\n"), 0o644))

		var copied struct {
			From  string   `json:"from"`
			To    string   `json:"to"`
			Files []string `json:"files"`
			Bytes int64    `json:"bytes"`
		}
		home := e.guestHome(t)
		remote := home + "/work"
		e.boxJSON(t, &copied, "cp", tree, testBox+":"+remote)
		require.Contains(t, copied.To, ":"+remote)

		res := e.boxOK(t, "run", testBox, "--", "cat", remote+"/a.txt")
		require.Equal(t, "alpha", strings.TrimSpace(res.stdout))
		res = e.boxOK(t, "run", testBox, "--", "cat", remote+"/sub/b.txt")
		require.Equal(t, "beta", strings.TrimSpace(res.stdout))

		// A target that does not exist takes the name of the copy: the tree
		// arrives as "back".
		out := filepath.Join(local, "back")
		e.boxOK(t, "cp", testBox+":"+remote, out)
		data, err := os.ReadFile(filepath.Join(out, "sub", "b.txt"))
		require.NoError(t, err)
		require.Equal(t, "beta\n", string(data))

		// A target directory with a trailing slash keeps the guest name.
		into := filepath.Join(local, "into")
		require.NoError(t, os.MkdirAll(into, 0o755))
		e.boxOK(t, "cp", testBox+":"+remote, into+"/")
		data, err = os.ReadFile(filepath.Join(into, "work", "a.txt"))
		require.NoError(t, err)
		require.Equal(t, "alpha\n", string(data))

		// A single file keeps its name.
		require.NoError(t, os.WriteFile(filepath.Join(local, "one.txt"), []byte("one\n"), 0o644))
		e.boxOK(t, "cp", filepath.Join(local, "one.txt"), testBox+":"+home+"/one.txt")
		single := filepath.Join(local, "copy.txt")
		e.boxOK(t, "cp", testBox+":"+home+"/one.txt", single)
		data, err = os.ReadFile(single)
		require.NoError(t, err)
		require.Equal(t, "one\n", string(data))

		// A missing path is a not-found, not a silent success.
		res = e.box(t, "cp", testBox+":"+home+"/absent", filepath.Join(local, "absent"))
		require.Equal(t, 3, res.code)
		require.Contains(t, res.stderr, "No such file")
	})

	t.Run("8 logs reads the task log stream", func(t *testing.T) {
		// The task log is the output of the guest log command
		// (`journalctl -f -o cat -u boxd`). This image has no boxd, and the
		// guest journalctl follows a unit that has no entries, so the log is
		// empty right now: the command must succeed and return quietly, not
		// fail. A fake journalctl is installed here; subtest 11 restarts the
		// box and proves that its line reaches `box logs`.
		res := e.box(t, "logs", testBox)
		require.Equal(t, 0, res.code, "box logs must not fail on an empty log: %s", res.stderr)

		local := t.TempDir()
		fake := filepath.Join(local, "journalctl")
		require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\necho 'fake boxd log line'\nexit 0\n"), 0o755))
		// Keep the real journalctl: scenario 11 needs it back, because boxd writes
		// its event feed into the journal.
		e.boxOK(t, "run", testBox, "--", "sudo", "-n", "cp", "-f", "/usr/bin/journalctl", "/usr/bin/journalctl.real")
		e.installInGuest(t, fake, "/usr/bin/journalctl")
		res = e.boxOK(t, "run", testBox, "--", "journalctl")
		require.Contains(t, res.stdout, "fake boxd log line", "the fake journalctl must run in the guest")

		// The driver starts the guest log command once per task attempt and
		// keeps following it, so a binary replaced under a running command is
		// not picked up. Stop and start the box to get a new attempt, then the
		// line of the fake journalctl has to appear in `box logs`.
		e.boxOK(t, "stop", testBox)
		e.boxOK(t, "start", testBox, "--timeout", "10m")
		require.Eventually(t, func() bool {
			logs := e.box(t, "logs", testBox)
			return logs.code == 0 && strings.Contains(logs.stdout, "fake boxd log line")
		}, 90*time.Second, 3*time.Second, "`box logs` never showed the guest log command output; client log tail: %s", e.clientLogTail())
	})

	t.Run("9 resize grows the vCPU count", func(t *testing.T) {
		var resized struct {
			Box struct {
				CPUs      int    `json:"cpus"`
				MemoryMiB int    `json:"memory_mib"`
				State     string `json:"state"`
			} `json:"box"`
			Changes []string `json:"changes"`
		}
		e.boxJSON(t, &resized, "resize", testBox, "--cpus", "2", "--timeout", "10m")
		require.Equal(t, 2, resized.Box.CPUs)
		require.Equal(t, "running", resized.Box.State)
		require.NotEmpty(t, resized.Changes)

		res := e.boxOK(t, "run", testBox, "--", "nproc")
		require.Equal(t, "2", strings.TrimSpace(res.stdout), "the guest sees the new vCPUs")

		// The disk cannot shrink.
		res = e.box(t, "resize", testBox, "--disk-gib", "4")
		require.Equal(t, 2, res.code)
		require.Contains(t, res.stderr, "can only grow")
	})

	t.Run("10 sessions through a fake boxctl in the guest", func(t *testing.T) {
		local := t.TempDir()
		script := filepath.Join(local, "boxctl")
		require.NoError(t, os.WriteFile(script, []byte(fakeBoxctl), 0o755))
		e.installInGuest(t, script, "/usr/local/bin/boxctl")

		var info struct {
			Guest *struct {
				BoxdVersion string `json:"boxd_version"`
				Sessions    int    `json:"sessions"`
				Executors   []struct {
					Name      string `json:"name"`
					Available bool   `json:"available"`
				} `json:"executors"`
			} `json:"guest"`
		}
		e.boxJSON(t, &info, "status", testBox)
		require.NotNil(t, info.Guest, "the fake boxctl answers boxctl info")
		require.Equal(t, "fake-0.1", info.Guest.BoxdVersion)
		require.Len(t, info.Guest.Executors, 1)

		var created struct {
			Session string `json:"session"`
			Details *struct {
				Executor string `json:"executor"`
				Model    string `json:"model"`
				Title    string `json:"title"`
			} `json:"details"`
		}
		e.boxJSON(t, &created, "session", "new", testBox, "--executor", "prime", "--model", "prov/model", "--title", "integration")
		require.Equal(t, "s-fake1", created.Session)
		require.NotNil(t, created.Details)
		require.Equal(t, "prime", created.Details.Executor)

		var list struct {
			Sessions []struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"sessions"`
		}
		e.boxJSON(t, &list, "session", "ls", testBox)
		require.Len(t, list.Sessions, 1)
		require.Equal(t, "s-fake1", list.Sessions[0].ID)

		var sent struct {
			Turn int `json:"turn"`
		}
		e.boxJSON(t, &sent, "send", testBox, "s-fake1", "please", "work")
		require.Equal(t, 1, sent.Turn)

		var turn struct {
			State string `json:"state"`
			Text  string `json:"text"`
		}
		e.boxJSON(t, &turn, "wait", testBox, "s-fake1", "--timeout", "2m")
		require.Equal(t, "done", turn.State)
		require.Equal(t, "fake answer", turn.Text)

		res := e.boxOK(t, "events", testBox, "s-fake1")
		require.Contains(t, res.stdout, "turn_start")

		// The error envelope of the contract: boxctl reports busy and exits 1.
		e.boxOK(t, "run", testBox, "--", "touch", "/tmp/boxctl-busy")
		res = e.box(t, "send", testBox, "s-fake1", "while busy")
		require.Equal(t, 1, res.code)
		require.Contains(t, res.stderr, "still working on a turn")
		e.boxOK(t, "run", testBox, "--", "rm", "-f", "/tmp/boxctl-busy")

		var cancelled struct {
			State string `json:"state"`
		}
		e.boxJSON(t, &cancelled, "cancel", testBox, "s-fake1")
		require.Equal(t, "cancelled", cancelled.State)

		e.boxOK(t, "session", "close", testBox, "s-fake1")
	})

	t.Run("11 the real boxd runs a session", func(t *testing.T) {
		// Phase 3's daemon, installed from this tree into the guest, with the fake
		// ACP agent standing in for prime-agent: no model is called, but every
		// step of the contract is real.
		boxd := os.Getenv("BOX_TEST_BOXD")
		fakeacp := os.Getenv("BOX_TEST_FAKEACP")
		repo := os.Getenv("BOX_TEST_REPO")
		if boxd == "" || fakeacp == "" || repo == "" {
			t.Skip("BOX_TEST_BOXD/BOX_TEST_FAKEACP are not set: phase 3's binaries were not built")
		}
		home := e.guestHome(t)

		// The disk's event feed goes to the journal, so put the real journalctl
		// back (scenario 8 replaced it to prove the log path).
		res := e.box(t, "run", testBox, "--", "sudo", "-n", "cp", "-f", "/usr/bin/journalctl.real", "/usr/bin/journalctl")
		require.Equal(t, 0, res.code, "the real journalctl was not preserved: %s%s", res.stdout, res.stderr)

		e.boxOK(t, "cp", boxd, testBox+":"+home+"/boxd")
		e.boxOK(t, "cp", filepath.Join(repo, "guest", "boxd-install.sh"), testBox+":"+home+"/boxd-install.sh")
		res = e.box(t, "run", testBox, "--", "sudo", "-n", "sh", home+"/boxd-install.sh", "--from", home+"/boxd")
		require.Equal(t, 0, res.code, "boxd did not install: %s%s", res.stdout, res.stderr)
		e.installInGuest(t, fakeacp, "/usr/local/bin/prime-agent")

		var status struct {
			Guest *struct {
				BoxdVersion string `json:"boxd_version"`
				Executors   []struct {
					Name      string `json:"name"`
					Available bool   `json:"available"`
					Reason    string `json:"reason"`
				} `json:"executors"`
				MemTotalMiB int64 `json:"mem_total_mib"`
				CPUs        int   `json:"cpus"`
			} `json:"guest"`
			GuestError string `json:"guest_error"`
		}
		e.boxJSON(t, &status, "status", testBox)
		require.NotNil(t, status.Guest, "the coordinator's own exec user must reach boxd: %s", status.GuestError)
		require.NotEmpty(t, status.Guest.BoxdVersion)
		require.Greater(t, status.Guest.MemTotalMiB, int64(0))
		require.GreaterOrEqual(t, status.Guest.CPUs, 1, "the guest reports its vCPUs")
		var primeAvailable bool
		for _, executor := range status.Guest.Executors {
			if executor.Name == "prime" {
				primeAvailable = executor.Available
				require.True(t, executor.Available, "prime is installed as a fake agent: %s", executor.Reason)
			}
		}
		require.True(t, primeAvailable, "the guest lists no prime executor")

		var created struct {
			Session string `json:"session"`
			Details *struct {
				Executor string `json:"executor"`
				Model    string `json:"model"`
				State    string `json:"state"`
			} `json:"details"`
		}
		e.boxJSON(t, &created, "session", "new", testBox, "--executor", "prime", "--model", "fake/model-1", "--title", "integration")
		require.NotEmpty(t, created.Session)
		require.NotNil(t, created.Details)
		require.Equal(t, "prime", created.Details.Executor)
		session := created.Session

		var sent struct {
			Turn int `json:"turn"`
		}
		e.boxJSON(t, &sent, "send", testBox, session, "pong")
		require.Equal(t, 1, sent.Turn)

		var turn struct {
			State      string `json:"state"`
			Text       string `json:"text"`
			StopReason string `json:"stop_reason"`
		}
		e.boxJSON(t, &turn, "wait", testBox, session, "--timeout", "2m")
		require.Equal(t, "done", turn.State)
		require.Contains(t, turn.Text, "pong", "the fake agent's answer reaches the coordinator")

		res = e.boxOK(t, "events", testBox, session)
		require.Contains(t, res.stdout, "turn_start")
		require.Contains(t, res.stdout, "turn_end")

		var list struct {
			Sessions []struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"sessions"`
		}
		e.boxJSON(t, &list, "session", "ls", testBox)
		require.Len(t, list.Sessions, 1)
		require.Equal(t, session, list.Sessions[0].ID)

		// boxd writes every event as one JSON line to its stdout, which the unit
		// redirects into the journal: the same events have to be in `box logs`,
		// which is the driver following `journalctl -u boxd` as the box user.
		asUser := e.box(t, "run", testBox, "--", "journalctl", "-u", "boxd", "--no-pager", "-n", "5")
		t.Logf("journal as the box user: %q", asUser.stdout+asUser.stderr)
		asRoot := e.box(t, "run", testBox, "--", "sudo", "-n", "journalctl", "-u", "boxd", "--no-pager", "-n", "5")
		t.Logf("journal as root: %q", asRoot.stdout+asRoot.stderr)
		require.Eventually(t, func() bool {
			logs := e.box(t, "logs", testBox)
			return logs.code == 0 && strings.Contains(logs.stdout, `"kind":"turn_end"`)
		}, 90*time.Second, 3*time.Second, "the boxd event feed never appeared in `box logs`; client log tail: %s", e.clientLogTail())

		e.boxOK(t, "session", "close", testBox, session)
	})

	t.Run("12 stop and start keep the disk", func(t *testing.T) {
		home := e.guestHome(t)
		e.boxOK(t, "run", testBox, "--", "sh", "-c", "echo kept > "+home+"/marker")

		var stopped struct {
			Box struct {
				State string `json:"state"`
			} `json:"box"`
		}
		e.boxJSON(t, &stopped, "stop", testBox)
		require.Equal(t, "stopped", stopped.Box.State)

		var ls struct {
			Boxes []struct {
				ID            string `json:"id"`
				State         string `json:"state"`
				DiskUsedBytes int64  `json:"disk_used_bytes"`
			} `json:"boxes"`
		}
		e.boxJSON(t, &ls, "ls")
		require.Len(t, ls.Boxes, 1, "a stopped box is still listed")
		require.Equal(t, "stopped", ls.Boxes[0].State)
		require.Greater(t, ls.Boxes[0].DiskUsedBytes, int64(0),
			"a stopped box keeps its disk, and Nomad still reports what it occupies")

		var started struct {
			Box struct {
				State string `json:"state"`
			} `json:"box"`
		}
		e.boxJSON(t, &started, "start", testBox, "--timeout", "10m")
		require.Equal(t, "running", started.Box.State)

		res := e.boxOK(t, "run", testBox, "--", "cat", home+"/marker")
		require.Equal(t, "kept", strings.TrimSpace(res.stdout), "the disk survived the stop")

		// The files copied in earlier are on the disk too.
		res = e.boxOK(t, "run", testBox, "--", "cat", home+"/work/a.txt")
		require.Equal(t, "alpha", strings.TrimSpace(res.stdout))
	})

	t.Run("13 rm deletes the job and the disk", func(t *testing.T) {
		var removed struct {
			Deleted bool `json:"deleted"`
			Box     struct {
				ID string `json:"id"`
			} `json:"box"`
		}
		e.boxJSON(t, &removed, "rm", testBox, "--yes")
		require.True(t, removed.Deleted)
		require.Equal(t, testBox, removed.Box.ID)

		var ls struct {
			Boxes []any `json:"boxes"`
		}
		e.boxJSON(t, &ls, "ls")
		require.Empty(t, ls.Boxes)

		res := e.box(t, "status", testBox)
		require.Equal(t, 3, res.code, "a deleted box is not found")
		require.Contains(t, res.stderr, "no box")

		// Nothing is left behind on the node.
		require.Empty(t, volumeNames(t, e), "the volume is deleted with the box")
	})

	t.Run("14 a lost register answer keeps the box and its disk", func(t *testing.T) {
		// The server takes the job and the answer never arrives. The CLI must
		// re-read the job before anything is deleted: a disk deleted under a
		// running box is a box that can never start again.
		proxy := newFaultProxy(t, e.addr)
		proxy.arm()

		res := e.boxAt(t, proxy.address(), "create", testBox2, "--image", imageVers,
			"--cpus", "1", "--memory-mib", "1024", "--disk-gib", fmt.Sprint(diskGiB), "--timeout", "10m")
		require.Equal(t, 0, res.code,
			"the CLI must reconcile the registration it could not see: %s%s; client log tail: %s",
			res.stdout, res.stderr, e.clientLogTail())
		require.True(t, proxy.droppedOnce(), "the injected failure did not happen: the answer was not lost")
		require.Contains(t, res.stdout+res.stderr, "registered, state",
			"the command must say that the job is registered anyway")
		require.Contains(t, res.stdout+res.stderr, "were kept")

		require.Contains(t, volumeNames(t, e), "box-"+testBox2,
			"the disk of a box Nomad holds must never be deleted")

		var list struct {
			Boxes []struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"boxes"`
		}
		e.boxJSON(t, &list, "ls")
		var state string
		for _, box := range list.Boxes {
			if box.ID == testBox2 {
				state = box.State
			}
		}
		require.Equal(t, "running", state, "the box the CLI reported as registered really runs")

		// The box is usable: it is a real box, not just a job record.
		res = e.boxOK(t, "run", testBox2, "--", "hostname")
		require.NotEmpty(t, strings.TrimSpace(res.stdout))
	})

	t.Run("15 rm then create the same id runs again", func(t *testing.T) {
		// `box rm` waits for Nomad to drop the volume registration, so the same
		// id can be created right after it.
		var removed struct {
			Deleted bool `json:"deleted"`
		}
		e.boxJSON(t, &removed, "rm", testBox2, "--yes")
		require.True(t, removed.Deleted)
		require.NotContains(t, volumeNames(t, e), "box-"+testBox2,
			"the volume registration is gone when `box rm` returns")

		var created struct {
			Box struct {
				ID    string `json:"id"`
				State string `json:"state"`
				Alloc string `json:"allocation_id"`
			} `json:"box"`
			Volume string `json:"volume"`
		}
		e.boxJSON(t, &created, "create", testBox2, "--image", imageVers,
			"--cpus", "1", "--memory-mib", "1024", "--disk-gib", fmt.Sprint(diskGiB), "--timeout", "10m")
		require.Equal(t, testBox2, created.Box.ID)
		require.Equal(t, "running", created.Box.State, "the new box with the old id runs")
		require.NotEmpty(t, created.Box.Alloc)
		require.NotEmpty(t, created.Volume)

		// It is a new disk, not the old one: the old box's work is gone.
		res := e.boxOK(t, "run", testBox2, "--", "sh", "-c", `test -e "$HOME/kept-marker" && echo old || echo new`)
		require.Contains(t, res.stdout, "new")

		// Leave the cluster as the suite found it.
		e.boxOK(t, "rm", testBox2, "--yes")
		require.Empty(t, volumeNames(t, e), "no volume is left behind")
	})
}

// faultProxy forwards every request to the real Nomad. When it is armed, the
// next job registration is sent and its answer is dropped: the server takes the
// job, and the client sees a broken answer -- the case the CLI has to reconcile.
type faultProxy struct {
	mu      sync.Mutex
	armed   bool
	dropped bool
	server  *httptest.Server
}

// newFaultProxy starts the proxy in front of target.
func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	upstream, err := url.Parse(target)
	require.NoError(t, err)
	fp := &faultProxy{}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		t.Logf("fault proxy: %s %s: %v", r.Method, r.URL.Path, err)
		w.WriteHeader(http.StatusBadGateway)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if resp.Request == nil || resp.Request.Method != http.MethodPut || resp.Request.URL.Path != "/v1/jobs" {
			return nil
		}
		fp.mu.Lock()
		defer fp.mu.Unlock()
		if !fp.armed || fp.dropped {
			return nil
		}
		fp.dropped = true
		// The upstream request has been answered by Nomad: only the answer to
		// the client is lost.
		return errors.New("injected: the answer to the job registration was lost")
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", faultProxyPort))
	require.NoError(t, err, "the fault proxy needs 127.0.0.1:%d (the suite's own port range)", faultProxyPort)
	server := httptest.NewUnstartedServer(proxy)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	fp.server = server
	return fp
}

// address is what NOMAD_ADDR points at while the proxy is in use.
func (p *faultProxy) address() string {
	return "http://" + p.server.Listener.Addr().String()
}

// arm makes the next job registration lose its answer.
func (p *faultProxy) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
	p.dropped = false
}

// droppedOnce reports whether an answer was dropped.
func (p *faultProxy) droppedOnce() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// fakeBoxctl is a guest boxctl that follows docs/boxctl-contract.md, so the
// session commands are tested end to end through real exec.
const fakeBoxctl = `#!/bin/sh
# A fake boxctl for the CLI integration suite.
case "$1" in
  info)
    echo '{"boxd_version":"fake-0.1","executors":[{"name":"prime","version":"9.9.9","available":true}],"sessions":1,"mem_total_mib":1024,"mem_free_mib":800,"cpus":1}'
    ;;
  session)
    case "$2" in
      new)
        echo '{"session":"s-fake1"}'
        ;;
      ls)
        echo '{"sessions":[{"id":"s-fake1","executor":"prime","model":"prov/model","title":"integration","state":"idle","last_turn":1,"created_at":"2026-11-02T10:00:00Z"}]}'
        ;;
      get)
        echo '{"id":"s-fake1","executor":"prime","model":"prov/model","cwd":"/root","title":"integration","state":"idle","last_turn":1,"created_at":"2026-11-02T10:00:00Z","executor_session":"root-1","pid":42}'
        ;;
      close)
        echo '{"session":"s-fake1","state":"closed"}'
        ;;
      *)
        echo '{"error":{"code":"bad_request","message":"unknown session subcommand"}}'
        exit 1
        ;;
    esac
    ;;
  send)
    if [ -f /tmp/boxctl-busy ]; then
      echo '{"error":{"code":"busy","message":"session s-fake1 is running turn 1"}}'
      exit 1
    fi
    cat > /dev/null
    echo '{"session":"s-fake1","turn":1}'
    ;;
  wait)
    echo '{"session":"s-fake1","turn":1,"state":"done","started_at":"2026-11-02T10:00:00Z","ended_at":"2026-11-02T10:00:05Z","stop_reason":"end_turn","text":"fake answer"}'
    ;;
  events)
    echo '{"events":[{"seq":1,"ts":"2026-11-02T10:00:00Z","session":"s-fake1","turn":1,"kind":"turn_start","summary":"started turn 1"},{"seq":2,"ts":"2026-11-02T10:00:05Z","session":"s-fake1","turn":1,"kind":"turn_end","summary":"answered"}],"next":3}'
    ;;
  cancel)
    echo '{"session":"s-fake1","turn":1,"state":"cancelled"}'
    ;;
  *)
    echo '{"error":{"code":"bad_request","message":"unknown command"}}'
    exit 1
    ;;
esac
`
