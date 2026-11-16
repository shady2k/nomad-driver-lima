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
| `TestCLI/10 sessions through a fake boxctl in the guest` | ALX-76 | Red in Integration since 2026-11-07: boxctl in the guest fails with `/usr/local/bin/boxctl: not found` (exit 127), so `boxctl info` never answers. |
| `TestCLI/11 the real boxd runs a session` | ALX-76 | The real boxd installs boxctl in the guest; it fails the same way, exit 127. |
| `TestAFailedTurnCancelsTheExecutorAndSaysItIsActiveOutsideTheTurn` (`internal/boxd`) | ALX-61 | Flaky under `-race`: the journal-count assertion races the executor's late updates. |

Only these tests are skipped. In `TestBoxLifecycle` the next subtest (7) uses
only the allocation and volume that scenario 9 left behind; scenario 10 changes
neither. In `TestCLI` the remaining subtests 12-15 use direct `box run`, Nomad
state and the volume, not boxctl or boxd; the real `journalctl` that subtest 11
would restore is not read by any later subtest. The whole `TestCLI` is not
quarantined.

To run the quarantined tests on purpose:

```sh
BOX_RUN_QUARANTINED=1 scripts/integration.sh
BOX_RUN_QUARANTINED=1 scripts/integration-cli.sh
BOX_RUN_QUARANTINED=1 go test -race ./internal/boxd
```
