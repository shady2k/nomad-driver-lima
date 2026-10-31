# Publication review

This tooling prepares a source snapshot for independent review. It never pushes,
creates a repository, changes licensing, or transfers private Git history.

1. Integrate the agreed driver and caller changes in an isolated checkout. Record
   the exact full commit and confirm no required work remains only uncommitted.
2. Review `manifest.json` against that commit. Every tracked file must be explicitly
   included or excluded. New or missing public paths stop export. Private deployment
   helpers, installation reports and old release receipts are excluded. Keep runtime
   assets and generic template helpers needed by the caller and offline tests.
3. Run `python3 tools/publication/export.py --source PATH --ref FULL_COMMIT
   --output NEW_DIRECTORY`. The output is a fresh directory without `.git`, old
   binaries, logs, caches, or untracked files. Keep the adjacent provenance report
   private; do not copy it into the public repository.
4. Run `verify.py NEW_DIRECTORY --private-patterns PRIVATE_JSON --report REPORT`.
   The private JSON is a list of regular expressions for the operator's actual
   domains, addresses, account paths and identifiers. Keep it outside the export.
   Review every reported path/category manually. No matched values are printed.
5. Run an established secret scanner against all export files and separately against
   all refs of the original repository. Use redacted reports outside the export.
   Classify each finding; fixture tokens, inert key markers and checksums can be
   false positives. Scanner success does not prove absence of secrets.
6. In the export, run offline unit tests, then build using a dedicated cache:
   `go test ./...`, `python3 -m unittest discover -s plugin/tests -p 'test_*.py'`,
   and `python3 -m unittest discover -s tools -p 'test_*.py'`. Inspect fixture
   subprocesses before running additional live integration scripts.
7. Rebuild with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath
   -buildvcs=false -o OUTPUT ./cmd/nomad-driver-lima` and repeat for darwin/arm64.
   Scan binaries (including strings/build metadata), source archives and caller
   archives. Record hashes, tool versions, platform and results outside the export.
8. Inspect all proposed public commit messages, author/committer identity, tags,
   workflow definitions and release metadata. Use an approved public author identity.
   A new snapshot must start in a fresh Git repository; do not push source refs.
9. Resolve project license and dependency obligations before publication. Submit
   exact target, candidate commit, diff, manifests, archive hashes and test/scanner
   results for independent review. Push only after that checkpoint is approved.

The manifest is deliberately conservative. Excluded deployment helpers remain in
private source; they are not deleted from any operator host. An excluded document
must not be linked from public documentation without providing a replacement.
