# Nomad Lima driver and nomad-workers

Current source snapshots, published as a prototype. This update preserves the
existing implementation; it does not redesign the system or claim an integrated
production-ready feature set.

## Source layout and status

| Component | Source revision | Location | Status |
|---|---|---|---|
| Linux driver | `60ecf69` | `cmd/`, `internal/`, root Go module | Frozen maintained Linux implementation |
| Darwin driver | `1a1ffe1` | `variants/darwin-driver/` | Separate frozen platform implementation; not merged with Linux |
| Caller core 0.1.48 | `1a351c97` | `variants/caller-core-0.1.48/plugin/` | Snapshot identified as installed by the source maintainer |
| Caller upload/shell candidate 0.1.49 | `25e42b8` | `plugin/` | Reviewed source candidate; not live accepted or activated |

The shared embedded repository helpers match across the selected driver and
caller revisions. The Linux and Darwin driver implementations have divergent
changes; they are deliberately preserved separately.

Broader preliminary work, including the caller 0.1.50 create/retry branch, is
preserved on `wip/preserved-current-source` under `wip/snapshots/`, with a source
inventory and explicit untested status. It is not silently merged into this main
snapshot. Private Git histories are not published.

## Known gaps

- Resize, destroy, template editing/publication and recovery remain distinct
  paths with incomplete or uneven interfaces. Their presence in source does not
  establish acceptance of every workflow.
- Nomad-only placement is not implemented as a unified architecture. Existing
  caller-side placement/gates remain in this source.
- Caller 0.1.49 adds scoped multipart upload and finite shell without requiring
  ACP readiness. It has source/offline coverage, not live environment acceptance.
- The Darwin snapshot is separate experimental platform code. No combined
  cross-platform production acceptance is claimed.
- Prepared-image acceptance, operational configuration and live deployment are
  outside this publication. No running VM or service was changed.

## Development

Build the root Linux driver with `go build ./cmd/nomad-driver-lima`. Build the
Darwin implementation from `variants/darwin-driver/` as a separate Go module.
Use `python3 -m unittest discover -s plugin/tests -p 'test_*.py'` for the caller's
offline tests. Platform/live tests require separately configured disposable
environments. No CI success is claimed by this source update.

The existing `linux-prototype-0.1.29` release is an older immutable snapshot. Its
binaries are not rebuilt or replaced by this source publication.

## Configuration and licensing

Use operator-owned configuration outside the repository for endpoints, node
identities, paths and credentials. Private inventories, receipts, tokens, logs,
build artifacts and installation-specific contexts are excluded.

Original code is Apache-2.0: see [LICENSE](LICENSE), [NOTICE](NOTICE), and
[third-party licensing](THIRD_PARTY.md). Dependencies retain their own licenses,
including Nomad's BUSL/MPL scopes.
