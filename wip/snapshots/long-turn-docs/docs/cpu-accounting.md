# CPU units and Apple Silicon detection

The driver consumes the Nomad agent's total compute and core count. CPU shares and
CPU ticks use that same scheduler scale; a guest vCPU count is a separate hard VM
setting. The driver does not maintain a machine-frequency table or silently repair
one node's units. Reservation uses integer ceil(total compute / cores), with zero
and overflow checks. Tests cover low native values and conventional MHz values.

An affected upstream combination is Nomad revision
`9dcbdc5e64ecc4ec63e42b225af729c87c87cf83` with `go-m1cpu v0.2.2` on Apple M4-family
hardware. The dependency's generation scanner uses `Apple M%`, which has no format
verb and returns an error. The generation remains zero, so the intended M4 kHz-to-Hz
conversion is skipped. Nomad subsequently converts the mislabeled value to MHz and
truncates it. This is upstream detection, not a Lima CPU limit or driver scheduler.

Sources:
- https://github.com/shoenig/go-m1cpu/blob/v0.2.2/cpu.go
- https://github.com/hashicorp/nomad/blob/9dcbdc5e64ecc4ec63e42b225af729c87c87cf83/client/lib/numalib/detect_darwin.go
- https://developer.hashicorp.com/nomad/docs/configuration/client#cpu_total_compute

A read-only local probe reproduced the parser error and compared the dependency
output with the exact IOKit clock properties. Installation-specific measurements
remain in private acceptance evidence, not this repository. The parser reproduction
is a fixture, not a patched or vendored CPU library.

Supported resolution: verify an upstream Nomad release with the corrected dependency,
or configure the official `client.cpu_total_compute` in external operator config
from independently verified frequencies and core counts. Keep the OS reserve separate.
For heterogeneous cores, nominal maximum compute is the sum of each class's core
count times its frequency in MHz. It is a capacity estimate, not measured performance
or a reason to allocate all host CPU without reserve.

Do not simply multiply an already truncated native total by 1000: precision was
lost per core before summing. Do not repair only the driver while the Nomad scheduler
continues admitting resources in a different scale. No live Nomad capacity, binary,
dependency, or operator configuration was changed by this investigation.

## Two-stage plugin startup

The Nomad plugin loader validates runtime config before CPU scanning is available.
Its SDK can deserialize a missing agent config as a non-nil empty object. This is
not a fatal topology error and is not capacity. SetConfig now validates/initializes
runtime config once, reports `compute.ready=false` and unhealthy while topology is
pending, and blocks StartTask. The driver manager's later valid topology activates
native accounting. A later empty topology closes admission again rather than invent
or retain stale capacity. Runtime configuration changes require a new process.

Regression uses the real official PluginLoader and a separate plugin process, with
an external CLI fixture that permits only version/list. It exercises early empty
config, later valid topology, reservation enforcement and return to pending; no VM
is started. Production activation still must verify that the actual driver manager
supplies valid topology and reports a healthy driver.
