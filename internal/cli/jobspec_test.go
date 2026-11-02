package cli

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// goodSpec is a valid box spec the tests copy and break.
func goodSpec() BoxSpec {
	return BoxSpec{
		ID:           "dev1",
		ImageVersion: "1.0.0",
		ImageURL:     "https://images.test/box-base/1.0.0/amd64/box.qcow2",
		ImageSHA256:  "3f9a2b7c5d1e0f3a4b6c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f809132435",
		Arch:         "amd64",
		CPUs:         2,
		MemoryMiB:    1024,
		DiskGiB:      8,
		NodeID:       "node-1",
	}
}

func TestRenderJobShape(t *testing.T) {
	job, err := RenderJob(goodSpec())
	require.NoError(t, err)

	require.Equal(t, "box-dev1", derefStr(job.ID, ""))
	require.Equal(t, "box-dev1", derefStr(job.Name, ""))
	require.Equal(t, "service", derefStr(job.Type, ""))
	require.Equal(t, []string{"*"}, job.Datacenters)

	// The meta block is how the command finds its boxes again.
	require.Equal(t, "true", job.Meta["box"])
	require.Equal(t, "dev1", job.Meta["box_id"])
	require.Equal(t, "1.0.0", job.Meta["image"])
	require.Equal(t, "amd64", job.Meta["arch"])

	require.Len(t, job.Constraints, 1)
	require.Equal(t, "${attr.driver.box.arch}", job.Constraints[0].LTarget)
	require.Equal(t, "=", job.Constraints[0].Operand)
	require.Equal(t, "x86_64", job.Constraints[0].RTarget, "the driver reports the lima spelling")

	require.Len(t, job.TaskGroups, 1)
	group := job.TaskGroups[0]
	require.Equal(t, "box", derefStr(group.Name, ""))
	require.Equal(t, 1, derefInt(group.Count, 0))
	require.NotNil(t, group.Disconnect)
	require.Equal(t, 10*time.Minute, *group.Disconnect.LostAfter)
	require.False(t, *group.Disconnect.Replace)
	require.Equal(t, "keep_original", string(*group.Disconnect.Reconcile))
	require.Equal(t, 3, *group.RestartPolicy.Attempts)
	require.Equal(t, "fail", *group.RestartPolicy.Mode)
	require.NotNil(t, group.Update, "a box needs longer than Nomad's 5 minute healthy deadline")
	require.Equal(t, 15*time.Minute, *group.Update.HealthyDeadline)
	require.Equal(t, 20*time.Minute, *group.Update.ProgressDeadline)
	require.Equal(t, 3*time.Second, *group.RestartPolicy.Delay)

	volume := group.Volumes["disk"]
	require.NotNil(t, volume)
	require.Equal(t, "host", volume.Type)
	require.Equal(t, "box-dev1", volume.Source)
	require.False(t, volume.ReadOnly)

	require.Len(t, group.Tasks, 1)
	task := group.Tasks[0]
	require.Equal(t, "box", task.Name)
	require.Equal(t, "box", task.Driver)
	require.Equal(t, "dev1", task.Config["box_id"])
	image := task.Config["image"].(map[string]any)
	require.Equal(t, goodSpec().ImageURL, image["url"])
	require.Equal(t, goodSpec().ImageSHA256, image["sha256"])
	require.Equal(t, 2, task.Config["cpus"])
	require.Equal(t, 1024, task.Config["memory_mib"])
	require.Equal(t, 8, task.Config["disk_gib"])
	require.Equal(t, "/etc/box", task.Config["files_dir_in_guest"])
	require.Equal(t, []string{"sudo", "-n", "journalctl", "-f", "-o", "cat", "-u", "boxd"}, task.Config["guest_log_command"])

	// Nomad reserves guest RAM plus the VM overhead, and 1000 MHz per vCPU.
	require.Equal(t, 1024+BoxMemoryOverheadMiB, derefInt(task.Resources.MemoryMB, 0))
	require.Equal(t, 2000, derefInt(task.Resources.CPU, 0))

	require.Len(t, task.VolumeMounts, 1)
	require.Equal(t, "disk", derefStr(task.VolumeMounts[0].Volume, ""))
	require.Equal(t, "/disk", derefStr(task.VolumeMounts[0].Destination, ""))
	require.Equal(t, boxStopTimeout, *task.KillTimeout)
}

func TestRenderJobArm64Constraint(t *testing.T) {
	spec := goodSpec()
	spec.Arch = "arm64"
	job, err := RenderJob(spec)
	require.NoError(t, err)
	require.Equal(t, "aarch64", job.Constraints[0].RTarget)
	require.Equal(t, "arm64", job.Meta["arch"], "the meta keeps the image spelling")
}

func TestRenderJobValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*BoxSpec)
		want   string
	}{
		{"bad id", func(s *BoxSpec) { s.ID = "Dev_1" }, "is not usable"},
		{"empty id", func(s *BoxSpec) { s.ID = "" }, "a box id is required"},
		{"no image", func(s *BoxSpec) { s.ImageURL = "" }, "no image is set"},
		{"no sha", func(s *BoxSpec) { s.ImageSHA256 = "" }, "no image is set"},
		{"bad arch", func(s *BoxSpec) { s.Arch = "sparc" }, "is not supported"},
		{"no arch", func(s *BoxSpec) { s.Arch = "" }, "no architecture"},
		{"no cpus", func(s *BoxSpec) { s.CPUs = 0 }, "--cpus must be at least 1"},
		{"small memory", func(s *BoxSpec) { s.MemoryMiB = 128 }, "--memory-mib must be at least 256"},
		{"no disk", func(s *BoxSpec) { s.DiskGiB = 0 }, "--disk-gib must be at least 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := goodSpec()
			tc.change(&spec)
			_, err := RenderJob(spec)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			require.Equal(t, ExitUsage, ExitCode(err))
		})
	}
}

func TestRenderVolume(t *testing.T) {
	volume, err := RenderVolume(VolumeSpec{BoxID: "dev1", NodeID: "node-1", DiskGiB: 8})
	require.NoError(t, err)
	require.Equal(t, "box-dev1", volume.Name)
	require.Equal(t, "box-volume", volume.PluginID)
	require.Equal(t, "node-1", volume.NodeID)
	require.Equal(t, int64(8)<<30, volume.RequestedCapacityMinBytes)
	require.Equal(t, int64(8)<<30, volume.RequestedCapacityMaxBytes, "the capacity is the disk")
	require.Len(t, volume.RequestedCapabilities, 1)
	require.Equal(t, "file-system", string(volume.RequestedCapabilities[0].AttachmentMode))
	require.Equal(t, "single-node-writer", string(volume.RequestedCapabilities[0].AccessMode))
	// A volume carries no parameters at all: limactl, lima_home and runtime_user
	// are the node's settings, and the box-volume plugin refuses a volume that
	// tries to set them.
	require.Empty(t, volume.Parameters, "the coordinator declares the volume and nothing else")

	_, err = RenderVolume(VolumeSpec{BoxID: "dev1", DiskGiB: 0})
	require.Error(t, err)
	require.Contains(t, err.Error(), "--disk-gib must be at least 1")
}

func TestSpecFromJobRoundTrip(t *testing.T) {
	spec := goodSpec()
	job, err := RenderJob(spec)
	require.NoError(t, err)
	back, err := SpecFromJob(job)
	require.NoError(t, err)
	require.Equal(t, spec.ID, back.ID)
	require.Equal(t, spec.ImageVersion, back.ImageVersion)
	require.Equal(t, spec.ImageURL, back.ImageURL)
	require.Equal(t, spec.ImageSHA256, back.ImageSHA256)
	require.Equal(t, spec.Arch, back.Arch)
	require.Equal(t, spec.CPUs, back.CPUs)
	require.Equal(t, spec.MemoryMiB, back.MemoryMiB)
	require.Equal(t, spec.DiskGiB, back.DiskGiB)
	require.Equal(t, BoxMemoryOverheadMiB, back.MemoryOverheadMiB)
	require.Equal(t, spec.MemoryMiB+BoxMemoryOverheadMiB, back.TaskMemoryMiB())
}

func TestSpecFromJobAcceptsNumbers(t *testing.T) {
	job, err := RenderJob(goodSpec())
	require.NoError(t, err)
	task := job.TaskGroups[0].Tasks[0]
	// The JSON round trip of a task config turns numbers into float64.
	task.Config["cpus"] = float64(4)
	task.Config["memory_mib"] = "2048"
	task.Config["guest_log_command"] = []any{"journalctl", "-u", "boxd"}
	back, err := SpecFromJob(job)
	require.NoError(t, err)
	require.Equal(t, 4, back.CPUs)
	require.Equal(t, 2048, back.MemoryMiB)
	require.Equal(t, []string{"journalctl", "-u", "boxd"}, back.GuestLogCommand)
}

func TestSpecFromJobNeedsABoxTask(t *testing.T) {
	job, err := RenderJob(goodSpec())
	require.NoError(t, err)
	job.TaskGroups[0].Tasks[0].Name = "other"
	_, err = SpecFromJob(job)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not a box")
}

func TestIsBoxJob(t *testing.T) {
	job, err := RenderJob(goodSpec())
	require.NoError(t, err)
	require.True(t, IsBoxJob(job))
	require.True(t, IsBoxJobStub(&api.JobListStub{ID: "box-dev1", Meta: job.Meta}))
	require.False(t, IsBoxJobStub(&api.JobListStub{ID: "other", Meta: map[string]string{}}))
	require.False(t, IsBoxJob(nil))
}

func TestBoxNames(t *testing.T) {
	require.Equal(t, "box-dev1", BoxJobName("dev1"))
	require.Equal(t, "box-dev1", BoxVolumeName("dev1"))
	id, ok := BoxIDFromJobName("box-dev1")
	require.True(t, ok)
	require.Equal(t, "dev1", id)
	_, ok = BoxIDFromJobName("other")
	require.False(t, ok)
}

func TestValidateBoxID(t *testing.T) {
	for _, id := range []string{"a", "dev1", "dev-1", "a1b2c3"} {
		require.NoError(t, ValidateBoxID(id), id)
	}
	for _, id := range []string{"", "-dev", "dev-", "Dev", "dev_1", "dev 1", "../../etc"} {
		require.Error(t, ValidateBoxID(id), id)
	}
}

func TestLimaArch(t *testing.T) {
	require.Equal(t, "x86_64", LimaArch("amd64"))
	require.Equal(t, "aarch64", LimaArch("arm64"))
	require.Equal(t, "aarch64", LimaArch("aarch64"))
}
