# Third-party components

The repository's Apache-2.0 license applies to original project code. Dependencies
retain their existing licenses. Their license texts and source availability
information are distributed in the release's `third-party-notices.tar.gz` sidecar.
No dependency source is relicensed by the project LICENSE or NOTICE.

The driver pins Nomad module
`v1.11.0-rc.1.0.20260917172403-9dcbdc5e64ec`. That module contains BUSL-1.1 source
files and separate MPL-2.0 scopes, including the plugin SDK. The SDK imports code
outside its MPL scopes. The compiled binary therefore cannot be described as
entirely Apache-2.0 or entirely MPL-2.0. The original BUSL parameters, including
its Additional Use Grant and version-dependent Change Date, remain applicable.

- [Pinned Nomad license](https://github.com/hashicorp/nomad/blob/9dcbdc5e64ec/LICENSE)
- [Pinned plugin SDK license](https://github.com/hashicorp/nomad/blob/9dcbdc5e64ec/plugins/LICENSE)
- [Corresponding Nomad source archive](https://proxy.golang.org/github.com/hashicorp/nomad/@v/v1.11.0-rc.1.0.20260917172403-9dcbdc5e64ec.zip)

The sidecar preserves upstream LICENSE/License, NOTICE, COPYING and COPYRIGHT
files, Go runtime/vendor notices, exact module versions and per-file SPDX headers
when present. `DEPENDENCIES.json` provides source archive URLs for the exact
unmodified module versions. `go-dependencies.sum` records Go module checksums.
Compiler dependency counts are not a count of packages after linker elimination.

The source/caller archives do not bundle Nomad, Lima, QEMU, Node, acpx, Prime,
OpenBao executables or a prepared VM image. Runtime dependencies downloaded by
installation recipes retain their own terms. Publishing a prepared VM image
requires a separate installed-package license inventory.
