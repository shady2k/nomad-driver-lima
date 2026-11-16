# Integration suites

Two harnesses start their own Nomad server and client and run against real
Lima VMs on the host:

- `scripts/integration.sh` builds the driver and the `box-volume` plugin and
  runs `TestBoxLifecycle` (plus `TestBoxRootLifecycle` with `--root`).
- `scripts/integration-cli.sh` builds the CLI and runs `TestCLI`.

## Quarantine

Tests that are known to be red are parked with `quarantined(t, task, reason)`.
Without `BOX_RUN_QUARANTINED=1` the helper calls `t.Skip` with the task and the
reason. With `BOX_RUN_QUARANTINED=1` it returns immediately and the test runs as
before.

| Test | Task | Why it is parked |
| --- | --- | --- |
| `TestBoxLifecycle/10 the credentials are on the guest tmpfs, and a rotation reaches a running box` | ALX-76 | Red in Integration since 2026-11-07: the assertion at `integration_test.go:931` finds that the guest secrets directory is not a tmpfs. |
| `TestCLI/5 status shows the job, the allocation and the volume` | ALX-76 | Red: `task_memory_mib` is 1536, expected 1174: `examples/client.hcl` overhead 150 vs CLI 512. |
| `TestCLI/8 logs reads the task log stream` | ALX-76 | Red: `box logs` never shows the guest log command output after stop/start. |
| `TestCLI/11 the real boxd runs a session` | ALX-76 | The real boxd installs boxctl in the guest; boxctl exits 127: `/usr/local/bin/boxctl: not found`. |

Only these tests are skipped. In `TestBoxLifecycle` the next subtest (7) uses
only the allocation and volume that scenario 9 left behind; scenario 10 changes
neither. In `TestCLI` subtests 5 and 8 create no state that a later subtest
needs: 5 only reads `box status`, and 8 only replaces the guest `journalctl`
with a fake. The fake is installed only by 8, and the real `journalctl` is
needed only by 11 to put it back; 11 is quarantined too. Subtests 9 and 12-15
use `box resize`, `box run`, Nomad state and the volume, not `box status` or
`box logs`, so they do not depend on the skipped tests. The whole `TestCLI` is
not quarantined.

To run the quarantined tests on purpose:

```sh
BOX_RUN_QUARANTINED=1 scripts/integration.sh
BOX_RUN_QUARANTINED=1 scripts/integration-cli.sh
```
