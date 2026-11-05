package driver

import (
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"
)

func TestVZServicePIDs(t *testing.T) {
	ps := `  21095 /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
  21462 /opt/homebrew/bin/limactl
  90750 /System/Library/Frameworks/WebKit.framework/Versions/A/XPCServices/com.apple.WebKit.WebContent.xpc/Contents/MacOS/com.apple.WebKit.WebContent
  21469 /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
`
	require.Equal(t, []int{21095, 21469}, vzServicePIDs(ps))
	require.Empty(t, vzServicePIDs(""))
}

func TestLsofHolds(t *testing.T) {
	lsof := "p21095\nfcwd\nn/\nf10\nn/Users/a/.box/volumes/x1/box-a.qcow2\nf11\nn/usr/lib/dyld\n"
	require.True(t, lsofHolds(lsof, []string{"/Users/a/.box/volumes/x1"}))
	require.True(t, lsofHolds(lsof, []string{"", "/Users/a/.box/volumes/x1/box-a.qcow2"}))
	// A sibling that shares the prefix is another box.
	require.False(t, lsofHolds(lsof, []string{"/Users/a/.box/volumes/x"}))
	require.False(t, lsofHolds(lsof, []string{"/Users/a/.box/lima/a", ""}))
}

func TestVMPIDIsAnAttributeOnceKnown(t *testing.T) {
	h := &taskHandle{tc: &drivers.TaskConfig{ID: "t"}, boxID: "b"}
	require.NotContains(t, h.taskStatus().DriverAttributes, "vm_pid")
	require.False(t, h.noteVMPID(0))
	require.True(t, h.noteVMPID(21469))
	require.False(t, h.noteVMPID(21469), "the same pid is no news")
	require.Equal(t, "21469", h.taskStatus().DriverAttributes["vm_pid"])
	require.True(t, h.noteVMPID(30000), "a restarted VM is")
}
