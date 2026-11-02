//go:build integration

// Package integration runs the box driver against real Lima VMs on this host.
//
// It is driven by scripts/integration.sh: a local Nomad server and client, the
// box driver and the box-volume plugin are already running when these tests
// start. Every scenario asserts that a marker file written inside the guest
// survives what the scenario does to the VM.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	boxID    = "box-it"
	jobName  = "box"
	volumeID = "boxdisk"
	taskName = "box"
)

// imageURL and imageSHA are set in TestMain: the cloud image is served from a
// local HTTP server so the driver downloads it through its real code path once.
var (
	imageURL string
	imageSHA string
	imageSrv *httptest.Server
)

type testEnv struct {
	nomadBin    string
	addr        string
	work        string
	limactl     string
	limaHome    string
	image       string
	examples    string
	startClient string
	clientPID   string
	nomadLog    string
	// runtimeUser is the unprivileged user the driver runs Lima as. It is the
	// driver's runtime_user setting, so the rendered job and volume have to agree
	// with the client configuration.
	runtimeUser string
}

func TestMain(m *testing.M) {
	image := os.Getenv("BOX_TEST_IMAGE")
	if image == "" {
		fmt.Fprintln(os.Stderr, "BOX_TEST_IMAGE is not set; run scripts/integration.sh")
		os.Exit(1)
	}
	dir := filepath.Dir(image)
	imageSrv = httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer imageSrv.Close()

	sum, err := fileSHA256(image)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hash image: %v\n", err)
		os.Exit(1)
	}
	imageSHA = sum
	imageURL = imageSrv.URL + "/" + filepath.Base(image)

	os.Exit(m.Run())
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func loadEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{
		nomadBin:    os.Getenv("BOX_TEST_NOMAD"),
		addr:        os.Getenv("BOX_TEST_NOMAD_ADDR"),
		work:        os.Getenv("BOX_TEST_WORK"),
		limactl:     os.Getenv("BOX_TEST_LIMACTL"),
		limaHome:    os.Getenv("BOX_TEST_LIMA_HOME"),
		image:       os.Getenv("BOX_TEST_IMAGE"),
		examples:    os.Getenv("BOX_TEST_EXAMPLES"),
		startClient: os.Getenv("BOX_TEST_START_CLIENT"),
		clientPID:   os.Getenv("BOX_TEST_CLIENT_PID"),
		nomadLog:    os.Getenv("BOX_TEST_NOMAD_LOG"),
		runtimeUser: os.Getenv("BOX_TEST_RUNTIME_USER"),
	}
	if e.runtimeUser == "" {
		if u, err := user.Current(); err == nil {
			e.runtimeUser = u.Username
		}
	}
	for name, value := range map[string]string{
		"BOX_TEST_NOMAD":        e.nomadBin,
		"BOX_TEST_NOMAD_ADDR":   e.addr,
		"BOX_TEST_WORK":         e.work,
		"BOX_TEST_LIMACTL":      e.limactl,
		"BOX_TEST_LIMA_HOME":    e.limaHome,
		"BOX_TEST_IMAGE":        e.image,
		"BOX_TEST_EXAMPLES":     e.examples,
		"BOX_TEST_START_CLIENT": e.startClient,
		"BOX_TEST_CLIENT_PID":   e.clientPID,
		"BOX_TEST_RUNTIME_USER": e.runtimeUser,
	} {
		require.NotEmpty(t, value, "%s must be set (run scripts/integration.sh)", name)
	}
	return e
}

// ---------- nomad and limactl plumbing ----------

func (e *testEnv) nomadCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(e.nomadBin, args...)
	cmd.Env = append(os.Environ(), "NOMAD_ADDR="+e.addr)
	return cmd
}

func (e *testEnv) nomad(t *testing.T, args ...string) string {
	t.Helper()
	cmd := e.nomadCmd(args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "nomad %s failed: %s", strings.Join(args, " "), out)
	return string(out)
}

func (e *testEnv) nomadTry(args ...string) (string, error) {
	out, err := e.nomadCmd(args...).CombinedOutput()
	return string(out), err
}

func (e *testEnv) lima(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(e.limactl, args...)
	cmd.Env = append(os.Environ(), "LIMA_HOME="+e.limaHome)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "limactl %s failed: %s", strings.Join(args, " "), out)
	return string(out)
}

type limaInstance struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Dir          string `json:"dir"`
	CPUs         int    `json:"cpus"`
	Memory       int64  `json:"memory"`
	Disk         int64  `json:"disk"`
	HostAgentPID int    `json:"hostAgentPID"`
	DriverPID    int    `json:"driverPID"`
}

func (e *testEnv) instances(t *testing.T) []limaInstance {
	t.Helper()
	out := e.lima(t, "list", "--format", "json")
	var list []limaInstance
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var inst limaInstance
		require.NoError(t, json.Unmarshal([]byte(line), &inst))
		list = append(list, inst)
	}
	return list
}

func (e *testEnv) instance(t *testing.T) *limaInstance {
	t.Helper()
	for _, inst := range e.instances(t) {
		if inst.Name == boxID {
			copied := inst
			return &copied
		}
	}
	return nil
}

type allocStatus struct {
	ID            string
	ClientStatus  string
	DesiredStatus string
}

func (e *testEnv) allocs(t *testing.T) []allocStatus {
	t.Helper()
	out := e.nomad(t, "job", "allocs", "-json", jobName)
	var allocs []allocStatus
	require.NoError(t, json.Unmarshal([]byte(out), &allocs), "decode allocs: %s", out)
	return allocs
}

// waitRunningAlloc waits for an allocation of the job that is running and is not
// in the exclude set.
func (e *testEnv) waitRunningAlloc(t *testing.T, timeout time.Duration, exclude ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := e.nomadTry("job", "allocs", "-json", jobName)
		if err == nil {
			var allocs []allocStatus
			if json.Unmarshal([]byte(out), &allocs) == nil {
				for _, a := range allocs {
					if a.ClientStatus != "running" || a.DesiredStatus != "run" {
						continue
					}
					if contains(exclude, a.ID) {
						continue
					}
					return a.ID
				}
				last = out
			}
		}
		time.Sleep(2 * time.Second)
	}
	status, _ := e.nomadTry("alloc", "status", boxID)
	logs, _ := os.ReadFile(e.work + "/client.log")
	require.FailNow(t, "no running allocation of "+jobName+" in time",
		"last alloc list: %s\nnode status: %s\nclient log tail: %s", last, status, tail(string(logs), 60))
	return ""
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}

func (e *testEnv) exec(t *testing.T, alloc, command string) string {
	t.Helper()
	return cleanGuestOutput(e.nomad(t, "alloc", "exec", "-task", taskName, alloc, "/bin/sh", "-c", command))
}

// cleanGuestOutput drops the log lines limactl and nomad print around a command
// so assertions see only the guest's own output.
func cleanGuestOutput(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "time=") && strings.Contains(trimmed, "level=") {
			continue
		}
		if strings.Contains(trimmed, "level=warning") || strings.Contains(trimmed, "level=fatal") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// allocRunning reports whether the allocation is running for the job.
func (e *testEnv) allocRunning(t *testing.T, allocID string) bool {
	t.Helper()
	for _, a := range e.allocs(t) {
		if a.ID == allocID {
			return a.ClientStatus == "running" && a.DesiredStatus == "run"
		}
	}
	return false
}

func (e *testEnv) waitFor(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "timed out waiting for "+what)
}

func (e *testEnv) waitLogs(t *testing.T, alloc, want string, timeout time.Duration) {
	t.Helper()
	var last string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := e.nomadTry("alloc", "logs", "-task", taskName, alloc)
		if err == nil {
			last = out
			if strings.Contains(out, want) {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "guest log line never appeared in the task log", "want %q, last logs: %s", want, last)
}

// ---------- guest helpers ----------

func writeMarker(t *testing.T, e *testEnv, alloc, token string) {
	t.Helper()
	e.exec(t, alloc, "echo -n "+token+" > $HOME/box-marker && sync")
}

func readMarker(t *testing.T, e *testEnv, alloc string) string {
	t.Helper()
	var last string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err := e.nomadTry("alloc", "exec", "-task", taskName, alloc, "/bin/sh", "-c", "cat $HOME/box-marker")
		if err == nil {
			if value := strings.TrimSpace(cleanGuestOutput(out)); value != "" {
				return value
			}
		}
		// The node can be briefly unreachable right after a client restart.
		last = out
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "could not read the guest marker", "last output: %s", last)
	return ""
}

func (e *testEnv) guestSh(t *testing.T, alloc, command string) string {
	t.Helper()
	return strings.TrimSpace(e.exec(t, alloc, command))
}

// ---------- job and volume rendering ----------

type jobParams struct {
	BoxID      string
	CPUs       int
	MemoryMiB  int
	DiskGiB    int
	LogCommand []string
}

func defaultJobParams() jobParams {
	return jobParams{
		BoxID:      boxID,
		CPUs:       2,
		MemoryMiB:  1024,
		DiskGiB:    8,
		LogCommand: []string{"sh", "-c", "echo guest-log-line; while true; do sleep 5; done"},
	}
}

// taskFileTemplates are the files Nomad renders into every box task of this
// suite: one job file (which belongs on the box disk), one credential and one
// credential that a Variable rotation re-renders without restarting the VM.
const taskFileTemplates = `
      template {
        destination = "local/it-local.txt"
        data        = "it-local"
      }

      template {
        destination = "secrets/it-secret.txt"
        data        = "it-secret"
      }

      template {
        destination = "secrets/rotating.txt"
        data        = "{{ if nomadVarExists \"nomad/jobs/box-it-secrets\" }}{{ with (nomadVar \"nomad/jobs/box-it-secrets\") }}{{ (index . \"token\").Value }}{{ end }}{{ end }}"
        change_mode = "noop"
      }

      volume_mount {`

// boxSecretsVariable is the Nomad Variable the rotating credential comes from.
const boxSecretsVariable = "nomad/jobs/box-it-secrets"

// guestSecretsDir is where the driver puts the task's secrets: a tmpfs.
const guestSecretsDir = "/run/box/secrets"

func hclList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func (e *testEnv) renderJob(t *testing.T, p jobParams) string {
	t.Helper()
	template, err := os.ReadFile(filepath.Join(e.examples, "box.nomad.hcl"))
	require.NoError(t, err)
	rendered := string(template)
	for old, new := range map[string]string{
		"__BOX_ID__":       p.BoxID,
		"__IMAGE_URL__":    imageURL,
		"__IMAGE_SHA256__": imageSHA,
		"__CPUS__":         strconv.Itoa(p.CPUs),
		"__MEMORY_MIB__":   strconv.Itoa(p.MemoryMiB),
		"__DISK_GIB__":     strconv.Itoa(p.DiskGiB),
		"__LOG_COMMAND__":  hclList(p.LogCommand),
	} {
		rendered = strings.ReplaceAll(rendered, old, new)
	}
	rendered = strings.Replace(rendered, "\n      volume_mount {", taskFileTemplates, 1)
	require.Contains(t, rendered, "local/it-local.txt", "the rendered job must carry the task files")
	path := filepath.Join(t.TempDir(), "box.nomad.hcl")
	require.NoError(t, os.WriteFile(path, []byte(rendered), 0o644))
	return path
}

func (e *testEnv) renderVolume(t *testing.T) string {
	t.Helper()
	template, err := os.ReadFile(filepath.Join(e.examples, "box.volume.hcl"))
	require.NoError(t, err)
	rendered := stripParameters(string(template))
	path := filepath.Join(t.TempDir(), "box.volume.hcl")
	require.NoError(t, os.WriteFile(path, []byte(rendered), 0o644))
	return path
}

// stripParameters removes a volume file's parameters block.
//
// A box volume carries no parameters: which limactl a node runs, which Lima home
// it uses and which user owns the volumes are the node's settings, and the
// box-volume plugin refuses a volume that tries to set them (it reads them from
// the settings file next to its binary, which scripts/integration.sh writes).
// The example file still shows the old shape until the deploy kit is updated.
func stripParameters(hcl string) string {
	var out []string
	skipping := false
	for _, line := range strings.Split(hcl, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "parameters") {
			skipping = true
		}
		if skipping {
			if strings.Contains(line, "}") && strings.HasPrefix(strings.TrimSpace(line), "}") {
				skipping = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// volumeHostPath returns the host directory of the dynamic host volume.
func (e *testEnv) volumeHostPath(t *testing.T) string {
	t.Helper()
	out := e.nomad(t, "volume", "status", "-type=host", "-json", volumeID)
	var vol struct {
		HostPath string `json:"HostPath"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &vol), "decode volume: %s", out)
	require.NotEmpty(t, vol.HostPath, "volume status has no host path: %s", out)
	return vol.HostPath
}

// httpGetJSON fetches a Nomad API path and decodes the JSON response.
func (e *testEnv) httpGetJSON(t *testing.T, path string, out any) error {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(e.addr + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type nodeStub struct {
	ID     string `json:"ID"`
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

type nodeFull struct {
	ID         string            `json:"ID"`
	Name       string            `json:"Name"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
}

// nodeAttrTry returns the ready node's attributes, or an error when the node is
// not reported yet (for example while the client restarts).
func (e *testEnv) nodeAttrTry() (map[string]string, error) {
	var nodes []nodeStub
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(e.addr + "/v1/nodes")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/nodes: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if !strings.EqualFold(n.Status, "ready") {
			continue
		}
		resp2, err := client.Get(e.addr + "/v1/node/" + n.ID)
		if err != nil {
			return nil, err
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET /v1/node/%s: %s", n.ID, resp2.Status)
		}
		var full nodeFull
		if err := json.NewDecoder(resp2.Body).Decode(&full); err != nil {
			return nil, err
		}
		if full.Attributes == nil {
			full.Attributes = map[string]string{}
		}
		return full.Attributes, nil
	}
	return nil, fmt.Errorf("no ready node")
}

// jobAllocStub is one allocation of the job as the HTTP API reports it.
type jobAllocStub struct {
	ID            string `json:"ID"`
	ClientStatus  string `json:"ClientStatus"`
	DesiredStatus string `json:"DesiredStatus"`
}

// jobAllocsAll returns every allocation of the job, including terminal ones.
func (e *testEnv) jobAllocsAll(t *testing.T) []jobAllocStub {
	t.Helper()
	var allocs []jobAllocStub
	err := e.httpGetJSON(t, "/v1/job/"+jobName+"/allocations?all=true", &allocs)
	require.NoError(t, err, "list allocations of %s", jobName)
	return allocs
}

// allocIDs returns the IDs of every allocation the job ever had.
func (e *testEnv) allocIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, a := range e.jobAllocsAll(t) {
		ids = append(ids, a.ID)
	}
	return ids
}

// allocClientStatus returns the client status of one allocation ("" if unknown).
func (e *testEnv) allocClientStatus(t *testing.T, allocID string) string {
	t.Helper()
	for _, a := range e.jobAllocsAll(t) {
		if a.ID == allocID {
			return a.ClientStatus
		}
	}
	return ""
}

// nodeReady reports whether any node is ready, i.e. whether the client is
// heartbeating to the server again.
func (e *testEnv) nodeReady(t *testing.T) bool {
	t.Helper()
	attrs, err := e.nodeAttrTry()
	return err == nil && attrs != nil
}

// ---------- the scenarios ----------

func TestBoxLifecycle(t *testing.T) {
	e := loadEnv(t)

	var (
		allocID  string
		jobFile  = e.renderJob(t, defaultJobParams())
		firstPID int
	)

	t.Run("1 create volume and job: VM runs, guest logs and the marker are there", func(t *testing.T) {
		// The rotating credential exists before the box starts, so the template
		// reads it on its first render and Nomad knows the box depends on it.
		e.nomad(t, "var", "put", boxSecretsVariable, "token=one")
		volumeFile := e.renderVolume(t)
		e.nomad(t, "volume", "create", volumeFile)
		volStatus := e.nomad(t, "volume", "status", "-type=host", volumeID)
		require.Contains(t, volStatus, "ready", "volume status: %s", volStatus)

		e.nomad(t, "job", "run", "-detach", jobFile)
		allocID = e.waitRunningAlloc(t, 8*time.Minute)

		inst := e.instance(t)
		require.NotNil(t, inst, "the driver must create a lima instance named %s", boxID)
		require.Equal(t, "Running", inst.Status)
		firstPID = inst.DriverPID
		require.NotZero(t, firstPID)

		// The disk lives in the volume and the instance directory symlinks it.
		volDir := e.volumeHostPath(t)
		disk := filepath.Join(volDir, "box-"+boxID+".qcow2")
		require.FileExists(t, disk)
		link := filepath.Join(e.limaHome, boxID, "disk")
		target, err := os.Readlink(link)
		require.NoError(t, err)
		require.Equal(t, disk, target)

		e.waitLogs(t, allocID, "guest-log-line", 4*time.Minute)
		writeMarker(t, e, allocID, "scenario-1")
		require.Equal(t, "scenario-1", readMarker(t, e, allocID))

		// The two task directories land in different places: job files on the box
		// disk, credentials on the guest's tmpfs.
		require.Equal(t, "it-local", e.guestSh(t, allocID, "sudo -n cat /etc/box/local/it-local.txt"))
		require.Equal(t, "it-secret", e.guestSh(t, allocID, "sudo -n cat "+guestSecretsDir+"/it-secret.txt"))
	})

	t.Run("2 kill -9 the nomad client: same allocation, VM not restarted", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		pidData, err := os.ReadFile(e.clientPID)
		require.NoError(t, err)
		clientPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		require.NoError(t, err)
		require.NoError(t, exec.Command("kill", "-9", strconv.Itoa(clientPID)).Run())
		time.Sleep(3 * time.Second)

		cmd := exec.Command(e.startClient)
		require.NoError(t, cmd.Start(), "restart the nomad client")
		go func() { _ = cmd.Wait() }()

		// The node comes back and the same allocation is restored.
		e.waitFor(t, "the node to reconnect", 3*time.Minute, func() bool {
			attrs, err := e.nodeAttrTry()
			return err == nil && attrs != nil
		})
		e.waitFor(t, "the allocation to be restored", 3*time.Minute, func() bool {
			out, err := e.nomadTry("job", "allocs", "-json", jobName)
			if err != nil {
				return false
			}
			var allocs []allocStatus
			if json.Unmarshal([]byte(out), &allocs) != nil {
				return false
			}
			for _, a := range allocs {
				if a.ID == allocID && a.ClientStatus == "running" {
					return true
				}
			}
			return false
		})

		// The same allocation and the same VM: a client restart must not cost the
		// box anything. RecoverTask is what makes this true.
		require.True(t, e.allocRunning(t, allocID), "the same allocation must be running after the client restart")
		inst := e.instance(t)
		require.NotNil(t, inst)
		require.Equal(t, "Running", inst.Status)
		require.Equal(t, firstPID, inst.DriverPID, "the VM must not be restarted by a client restart")
		require.Equal(t, "scenario-1", readMarker(t, e, allocID))
	})

	t.Run("3 kill -9 the hostagent: nomad recovers the VM, same disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		inst := e.instance(t)
		require.NotNil(t, inst)
		hostAgent := inst.HostAgentPID
		require.NotZero(t, hostAgent)
		require.NoError(t, exec.Command("kill", "-9", strconv.Itoa(hostAgent)).Run())
		time.Sleep(2 * time.Second)

		// The driver reports the dead VM, Nomad restarts the task in the same
		// allocation and StartTask force-stops and starts the box.
		e.waitFor(t, "the box to be running again", 6*time.Minute, func() bool {
			current := e.instance(t)
			return current != nil && current.Status == "Running" && current.DriverPID != 0 && current.DriverPID != inst.DriverPID
		})
		e.waitFor(t, "the allocation to run again", 3*time.Minute, func() bool {
			out, err := e.nomadTry("job", "allocs", "-json", jobName)
			if err != nil {
				return false
			}
			return strings.Contains(out, allocID) && strings.Contains(out, `"running"`)
		})
		require.Equal(t, "scenario-1", readMarker(t, e, allocID), "the disk must survive a hostagent crash")
	})

	t.Run("4 job stop then run again: the new allocation adopts the same VM and disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		// Read the marker while the task still runs: once the job is stopped there
		// is no task to exec into.
		markerBefore := readMarker(t, e, allocID)

		e.nomad(t, "job", "stop", "-detach", jobName)
		e.waitFor(t, "the VM to stop", 3*time.Minute, func() bool {
			inst := e.instance(t)
			return inst != nil && inst.Status == "Stopped"
		})
		// The box disk is a single-writer host volume: the old allocation has to
		// be terminal before the next allocation may claim it.
		e.waitFor(t, "the old allocation to stop", 3*time.Minute, func() bool {
			for _, a := range e.allocs(t) {
				if a.ClientStatus == "running" || a.ClientStatus == "pending" {
					return false
				}
			}
			return true
		})

		e.nomad(t, "job", "run", "-detach", jobFile)
		newAlloc := e.waitRunningAlloc(t, 6*time.Minute, allocID)
		require.NotEqual(t, allocID, newAlloc, "a job run after stop must create a new allocation")
		require.Equal(t, markerBefore, readMarker(t, e, newAlloc))
		require.FileExists(t, filepath.Join(e.volumeHostPath(t), "box-"+boxID+".qcow2"))
		allocID = newAlloc
	})

	t.Run("5 change cpus/memory/disk: the VM is resized and the guest sees it", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		resized := defaultJobParams()
		resized.CPUs = 4
		resized.MemoryMiB = 1536
		resized.DiskGiB = 12
		before := allocID
		e.nomad(t, "job", "run", "-detach", e.renderJob(t, resized))
		allocID = e.waitRunningAlloc(t, 8*time.Minute, before)
		require.NotEqual(t, before, allocID)

		// Wait until the guest itself reports the new values.
		e.waitFor(t, "the guest to see 4 cpus", 4*time.Minute, func() bool {
			return e.guestSh(t, allocID, "nproc") == "4"
		})
		memTotal := e.guestSh(t, allocID, "free -m | awk '/^Mem:/ {print $2}'")
		memMiB, err := strconv.Atoi(strings.TrimSpace(memTotal))
		require.NoError(t, err, "free -m output: %q", memTotal)
		require.GreaterOrEqual(t, memMiB, 1400, "guest memory should be about 1536 MiB, got %d", memMiB)
		df := e.guestSh(t, allocID, "df -m / | awk 'NR==2 {print $2}'")
		rootMiB, err := strconv.ParseInt(strings.TrimSpace(df), 10, 64)
		require.NoError(t, err, "df output: %q", df)
		// A 12 GiB disk leaves about 10.6 GiB for the root filesystem; the 8 GiB
		// disk the box started with had about 6.8 GiB.
		require.Greater(t, rootMiB, int64(9*1024), "guest root filesystem should have grown with the disk, got %d MiB", rootMiB)
		require.Equal(t, "scenario-1", readMarker(t, e, allocID))
	})

	t.Run("6 the qemu process runs in a memory cgroup with swap disabled", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		inst := e.instance(t)
		require.NotNil(t, inst)
		require.NotZero(t, inst.DriverPID)
		cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", inst.DriverPID))
		require.NoError(t, err)
		require.Contains(t, string(cgroup), "box-"+boxID+".scope", "qemu must run inside the box's systemd scope: %s", cgroup)

		// cgroup v2 writes relative paths into /proc/<pid>/cgroup.
		rel := strings.TrimSpace(strings.SplitN(string(cgroup), "::", 2)[1])
		memoryMax, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", rel, "memory.max"))
		require.NoError(t, err)
		wantBytes := int64(1536+150) * 1024 * 1024
		require.Equal(t, strconv.FormatInt(wantBytes, 10), strings.TrimSpace(string(memoryMax)))

		swapMax, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", rel, "memory.swap.max"))
		require.NoError(t, err)
		require.Equal(t, "0", strings.TrimSpace(string(swapMax)))
	})

	t.Run("8 orphans attribute lists an instance created by hand", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		// The hand-made instance uses the test image too, and Lima refuses to
		// shrink a disk: its disk must not be smaller than the image's virtual
		// size (8 GiB for the box base image), or 'limactl create' fails with
		// "disk shrinking is not supported".
		orphanYAML := filepath.Join(t.TempDir(), "orphan.yaml")
		require.NoError(t, os.WriteFile(orphanYAML, []byte(fmt.Sprintf(`minimumLimaVersion: 2.0.0
vmType: qemu
arch: x86_64
images:
  - location: %q
    arch: x86_64
cpus: 1
memory: 1GiB
disk: 20GiB
mounts: []
containerd:
  system: false
  user: false
`, e.image)), 0o644))
		e.lima(t, "create", "--name", "orphan-box", "--tty=false", orphanYAML)
		t.Cleanup(func() {
			cmd := exec.Command(e.limactl, "delete", "-f", "orphan-box")
			cmd.Env = append(os.Environ(), "LIMA_HOME="+e.limaHome)
			_ = cmd.Run()
		})

		var orphans string
		e.waitFor(t, "the orphan to be reported", 2*time.Minute, func() bool {
			attrs, err := e.nodeAttrTry()
			if err != nil {
				return false
			}
			orphans = attrs["driver.box.orphans"]
			return strings.Contains(orphans, "orphan-box")
		})
		require.Contains(t, orphans, "orphan-box")
		require.NotContains(t, orphans, boxID, "a box with a task is not an orphan")
	})

	t.Run("9 the client is away past the heartbeat grace: same allocation, same VM, no replacement", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		inst := e.instance(t)
		require.NotNil(t, inst)
		pidBefore := inst.DriverPID
		allocsBefore := e.allocIDs(t)
		writeMarker(t, e, allocID, "scenario-9")

		// A real network blip is longer than the server's heartbeat grace: the
		// server marks the node down and the allocation unknown, which is the
		// case where a naive driver lets Nomad start a second VM with an empty
		// disk. Nomad's own defaults put the node down about 20 s after the last
		// heartbeat (client TTL 10 s + heartbeat_grace 10 s).
		pidData, err := os.ReadFile(e.clientPID)
		require.NoError(t, err)
		clientPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		require.NoError(t, err)
		require.NoError(t, exec.Command("kill", "-9", strconv.Itoa(clientPID)).Run())

		e.waitFor(t, "the server to mark the node down", 4*time.Minute, func() bool {
			return !e.nodeReady(t)
		})
		e.waitFor(t, "the server to mark the allocation unknown", 3*time.Minute, func() bool {
			return e.allocClientStatus(t, allocID) == "unknown"
		})
		for _, id := range e.allocIDs(t) {
			require.Contains(t, allocsBefore, id, "no replacement allocation may be created while the client is away")
		}

		// Bring the client back.
		cmd := exec.Command(e.startClient)
		require.NoError(t, cmd.Start(), "restart the nomad client")
		go func() { _ = cmd.Wait() }()

		e.waitFor(t, "the node to be ready again", 3*time.Minute, func() bool {
			return e.nodeReady(t)
		})
		e.waitFor(t, "the allocation to run again", 3*time.Minute, func() bool {
			return e.allocRunning(t, allocID)
		})

		for _, id := range e.allocIDs(t) {
			require.Contains(t, allocsBefore, id, "the client coming back must not create a replacement allocation")
		}
		inst = e.instance(t)
		require.NotNil(t, inst)
		require.Equal(t, "Running", inst.Status)
		require.Equal(t, pidBefore, inst.DriverPID, "the VM must not be restarted after a network blip")
		require.Equal(t, "scenario-9", readMarker(t, e, allocID))
	})

	t.Run("10 the credentials are on the guest tmpfs, and a rotation reaches a running box", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		if !e.allocRunning(t, allocID) {
			allocID = e.waitRunningAlloc(t, 3*time.Minute)
		}
		inst := e.instance(t)
		require.NotNil(t, inst)
		pidBefore := inst.DriverPID

		// The secrets directory is a tmpfs, root-owned, mode 0700, and the box
		// user cannot read it.
		require.Equal(t, "tmpfs", e.guestSh(t, allocID, "findmnt -no FSTYPE --target "+guestSecretsDir))
		require.Equal(t, "700", e.guestSh(t, allocID, "sudo -n stat -c %a "+guestSecretsDir))
		require.Equal(t, "root", e.guestSh(t, allocID, "sudo -n stat -c %U "+guestSecretsDir))
		out, err := e.nomadTry("alloc", "exec", "-task", taskName, allocID, "/bin/sh", "-c", "cat "+guestSecretsDir+"/it-secret.txt")
		require.Error(t, err, "the box user must not read the credentials: %s", out)

		// Nothing of it is on the box disk: /run is a different filesystem, so
		// find on / cannot see the tmpfs copy (or the credential at all).
		require.Empty(t, e.guestSh(t, allocID, "sudo -n find / -xdev -name it-secret.txt 2>/dev/null"))
		require.Empty(t, e.guestSh(t, allocID, "sudo -n find / -xdev -name rotating.txt 2>/dev/null"))
		require.FileExists(t, filepath.Join(e.volumeHostPath(t), "box-"+boxID+".qcow2"))

		// An image without boxd (the plain cloud image CI boots) gets the one
		// from this tree, through the installer an operator uses.
		if e.guestSh(t, allocID, "command -v boxd || true") == "" {
			boxd := os.Getenv("BOX_TEST_BOXD")
			if boxd == "" {
				t.Skip("the image has no boxd and BOX_TEST_BOXD is not set")
			}
			e.lima(t, "copy", boxd, boxID+":/tmp/boxd")
			e.lima(t, "copy", filepath.Join("..", "guest", "boxd-install.sh"), boxID+":/tmp/boxd-install.sh")
			e.guestSh(t, allocID, "sudo -n sh /tmp/boxd-install.sh --from /tmp/boxd >/dev/null")
		}

		// boxd reads the credentials from there and keeps its own copy in
		// /run/boxd, also a tmpfs.
		require.Equal(t, "tmpfs", e.guestSh(t, allocID, "findmnt -no FSTYPE --target /run/boxd"))
		require.Contains(t, e.guestSh(t, allocID, "boxctl reload"), "configured")

		// The driver fingerprints the live VM with whether a task owns it, which
		// is what `box capacity` subtracts the unaccounted VMs with.
		var attrs map[string]string
		e.waitFor(t, "the instance attributes", 2*time.Minute, func() bool {
			found, err := e.nodeAttrTry()
			if err != nil {
				return false
			}
			attrs = found
			return attrs["driver.box.instance."+boxID+".task"] == "running"
		})
		require.Equal(t, "Running", attrs["driver.box.instance."+boxID+".state"])
		// The fingerprint describes the VM as it is, not as the job first asked:
		// scenario 5 resized this box, so the reference is the live instance.
		require.Equal(t, strconv.Itoa(inst.CPUs), attrs["driver.box.instance."+boxID+".cpus"])

		// Rotation: the Variable changes, Nomad re-renders the template, the
		// driver copies it into the guest again and tells boxd. No new allocation,
		// no new VM.
		// -force: the variable exists (scenario 1 created it), and a rotation is
		// an overwrite, not a create.
		// The disk marker an earlier scenario wrote: the rotation must leave the
		// disk exactly as it was, whatever the marker says by now.
		markerBefore := readMarker(t, e, allocID)
		e.nomad(t, "var", "put", "-force", boxSecretsVariable, "token=two")
		e.waitFor(t, "the rotated credential in the guest", 2*time.Minute, func() bool {
			return e.guestSh(t, allocID, "sudo -n cat "+guestSecretsDir+"/rotating.txt") == "two"
		})
		require.Equal(t, pidBefore, e.instance(t).DriverPID, "a credential rotation must not restart the VM")
		require.True(t, e.allocRunning(t, allocID), "a credential rotation must not replace the allocation")
		require.Equal(t, markerBefore, readMarker(t, e, allocID), "a credential rotation must not touch the disk")
	})

	t.Run("7 volume delete after stopping the job removes the instance and the disk", func(t *testing.T) {
		if allocID == "" {
			t.Skip("scenario 1 has to pass first")
		}
		volDir := e.volumeHostPath(t)

		// A volume can only be deleted when no allocation uses it, so the job is
		// stopped first. Purging the job would take the volume with it (Nomad
		// deletes the volumes a purged job leaves behind), which is not what this
		// scenario is about.
		e.nomad(t, "job", "stop", "-detach", jobName)
		e.waitFor(t, "the allocation to stop", 3*time.Minute, func() bool {
			for _, a := range e.allocs(t) {
				if a.ClientStatus == "running" {
					return false
				}
			}
			return true
		})

		e.nomad(t, "volume", "delete", "-type=host", volumeID)
		e.waitFor(t, "the volume directory to be removed", 3*time.Minute, func() bool {
			_, err := os.Stat(volDir)
			return os.IsNotExist(err)
		})
		_, err := os.Stat(filepath.Join(e.limaHome, boxID))
		require.True(t, os.IsNotExist(err), "the lima instance must be deleted with its volume")

		e.nomad(t, "job", "stop", "-detach", "-purge", jobName)
	})

}
