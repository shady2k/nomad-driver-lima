package box

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCheckSocketPath keeps lima_home short enough for ssh's ControlMaster
// socket, ssh.sock.<16 characters>, under the longest box id.
func TestCheckSocketPath(t *testing.T) {
	require.NoError(t, CheckSocketPath("/var/lib/box/lima", ""))

	long := "/Users/alice/Library/Application Support/box-node/lima"
	err := CheckSocketPath(long, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "choose a shorter lima_home")
	require.Contains(t, err.Error(), "box ids of at most")

	// The limit is the kernel's, with the NUL: a path of exactly max fits.
	max := socketPathMax()
	home := "/" + strings.Repeat("h", 1+max-len(limaSocketPath("/h", "b1")))
	require.Len(t, limaSocketPath(home, "b1"), max)
	require.NoError(t, CheckSocketPath(home, "b1"))
	require.Error(t, CheckSocketPath(home+"h", "b1"))
}

// TestBoxRefusesALimaHomeTooLongForItsID refuses the box before Lima runs.
func TestBoxRefusesALimaHomeTooLongForItsID(t *testing.T) {
	m := &Manager{opts: Options{LimaHome: "/" + strings.Repeat("h", 120)}}
	_, err := m.Box(Spec{
		BoxID: "dev1", VolumeDir: "/v", ImageURL: "file:///i", ImageSHA256: strings.Repeat("0", 64),
		CPUs: 1, MemoryMiB: 512, DiskGiB: 8,
	}, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidSpec))
	require.Contains(t, err.Error(), "lima_home")
}

func TestMaxBoxIDFor(t *testing.T) {
	require.Equal(t, MaxBoxIDLen, MaxBoxIDFor("/var/lib/box/lima"))
	require.Equal(t, 0, MaxBoxIDFor("/"+strings.Repeat("h", 120)))
	home := "/" + strings.Repeat("h", 1+socketPathMax()-len(limaSocketPath("/h", "abcde")))
	require.Equal(t, 5, MaxBoxIDFor(home))
	require.NoError(t, CheckSocketPath(home, "abcde"))
	require.Error(t, CheckSocketPath(home, "abcdef"))
}
