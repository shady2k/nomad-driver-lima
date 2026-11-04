package boxd

import (
	"fmt"
	"os"
)

// relaxOOM gives a process boxd started the ordinary OOM score. boxd runs with
// OOMScoreAdjust=-800 so the kernel kills it last, and a child inherits that:
// without this an agent's runaway build would be spared and the kernel would
// kill something the box needs instead. Raising the score needs no privilege.
func relaxOOM(pid int) {
	if pid <= 0 {
		return
	}
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("0"), 0o644)
}
