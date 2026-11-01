# Offline preparation for the native ARM64 base

This workflow is prepared in source. It has not built or booted an ARM64 image.
Production Mac jobs, Nomad configuration and credentials remain untouched.

The reviewed Ubuntu recipe already contains ARM64 URLs and SHA256 for the same
Prime 0.10.1, acpx 0.21.0, Node 24.22.1 and Bao 2.5.4 versions as the Linux base.
Render it with `deploy/render-base-recipe.py` to 2 CPUs, 2048 MiB and 8 GiB, using
its reviewed source SHA. Keep inventory, operator paths and creator/session values
outside the repository; `deploy/base-mac.vars.example.yml` lists the parameters.
Use the exact authoritative frozen guest manifest, never mutable working-tree bytes.

Lima 2.2.1's [VZ driver](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/driver/vz/vz_driver_darwin.go)
uses raw for Linux guests and calls [EnsureDisk](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/driverutil/disk.go)
to create the `disk` file. No root-disk export subcommand exists in this release.
Our export reads only the normally stopped clean builder's standalone raw disk;
it does not clone arbitrary instance directories, copy firmware identity or copy
an old diffdisk/backing chain. ASIF, symlinks and legacy disk layouts fail closed.

Before a permitted runtime run, recheck the existing Mac jobs and available Nomad
reservations. The read-only Mac guard checks native architecture, physical disk
and memory pressure. The disk requirement is 6 × requested disk plus 20 GiB host
reserve: builder, immutable export, two retained acceptance disks, source cache and
temporary disk, or 68 GiB for the 8 GiB image. This is an explicit planning bound,
not a filesystem quota. Inspect cache/log growth at each milestone and stop before
the reserve is approached. Memory free percentage is advisory, not a reservation;
2 GiB must also fit the freshly checked per-node reservation headroom.

After explicit runtime approval, the order is:

1. `deploy/base-build.yml`: private native VZ builder, pinned rendered recipe;
   durable asynchronous install receipt and timestamped logs, no production restart.
2. `deploy/base-assets.yml`: install only the three reviewed frozen guest files.
3. `deploy/base-artifact.yml` with `base_phase=seal`, then `export`.
4. `boot`, `check`, `stop` with `base_boot_index=1`; repeat with index 2.
   VZ requires an explicit index and refuses to start while the other acceptance
   VM is not stopped. The same immutable export is used for both boots.
5. `catalog` only after both proofs: unique machine/boot/host/SSH identities,
   expected guest SHA, native runtime/ACP readiness, complete redacted output and
   unchanged image digest. This creates local metadata only; no S3 publication.

Raw export verifies the primary/backup GPT header checksums and exact expected size,
exact stopped Lima identity and config, PID absence and no other disk opener.
It copies to an exclusive partial file, preserves holes, fsyncs and compares source
before/after plus exported SHA before publishing. A partial/unknown outcome is not
retried automatically. Two fresh boots remain the required portability acceptance;
unit tests cannot prove that gate. QEMU keeps its existing checked qcow2 path.

Rollback of preparation means normally stopping only the identified own builder or
acceptance VM in the isolated Lima home, retaining its disk/logs/receipts for review.
Do not purge a partial, rebuild over it, stop production jobs or edit ledgers.
A failed check does not authorize changing permissions or lowering capacity guards.

The later production Mac change is separate: a reviewed Lima plugin configuration,
accepted ARM64/VZ catalog, explicit node disk budget and native Vault client settings.
Keep the BoxLite driver while its three jobs run. Any client restart must be separately
coordinated, with recovery/disruption assessed; this document does not authorize it.
No global Lima installation, PATH change, new account, security grant, model call or
credential delivery is part of this offline source work.
