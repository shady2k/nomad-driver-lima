# Dependency notices

This tooling records dependency licensing without changing the project license.
Use the exact same source commit, Go toolchain, CGO setting, platform and tags as
the binaries. Dependency closure counts are compiler-input counts; linker
elimination can remove code. Individual source SPDX headers are recorded because
a module root license alone does not describe mixed-license modules.

For each target, run `go list -deps -json ./cmd/nomad-driver-lima` with the build's
`CGO_ENABLED=0`, `GOOS` and `GOARCH`, then pass that private intermediate JSON to:

```
python3 tools/publication/licenses.py PLATFORM_PACKAGES.json NEW_NOTICE_DIRECTORY
```

The output omits local cache paths and preserves upstream LICENSE/License,
COPYING, NOTICE and COPYRIGHT files byte for byte. Standard-library and vendor
notices, including the Go runtime license, are included. Inspect missing notices
and resolve them before packaging. No upstream module source is modified.

Merge approved results with `package_notices.py --platform linux-amd64=PATH
--platform darwin-arm64=PATH --go-sum go.sum --artifact-hashes PRIVATE_HASHES.json
--output NEW_DISTRIBUTION_DIRECTORY`. Package only that output directory, not the
private `go list` JSON or investigation reports. Scan the result and retain hashes.

Nomad's pinned SDK has MPL files but imports BUSL files outside those scopes.
Removing a direct statistics import does not remove the SDK's transitive imports.
The bundle preserves the exact pinned Nomad license parameters and MPL source
locations. It does not claim that the whole binary is covered by an original-code
project license. Runtime/image recipe dependencies are separate from shipped Go
binaries; a VM image requires an additional installed-package license inventory.
