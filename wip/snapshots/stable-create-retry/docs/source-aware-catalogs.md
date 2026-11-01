# Source-aware native catalogs

Optional caller configuration can add separately pinned catalogs while retaining
its existing primary `catalog_file` and `catalog_sha256` unchanged:

```json
{
  "catalog_file": "/PRIVATE/current-catalog.json",
  "catalog_sha256": "REPLACE_WITH_EXISTING_PRIMARY_SHA256",
  "catalog_sources": [
    {
      "id": "native-arm",
      "catalog_file": "/PRIVATE/accepted-arm64-catalog.json",
      "catalog_sha256": "REPLACE_WITH_ACCEPTED_ARM64_CATALOG_SHA256"
    }
  ]
}
```

These are private operator inputs. Source files must be absolute local paths and
match their whole-file SHA256 pins. No artifact URL, source file path, credential
or arbitrary source metadata is returned by discovery. Every visible variant is
annotated with its source ID and catalog pin. Conflicting bytes for the same
release/platform identity are rejected; no implicit release-version aliases are
introduced. A request's template ID and version must exist in its selected source.

The caller lists resources per node and recognizes that node's own catalog pin.
Unknown pins or unknown effective capacity never become fabricated free slots.
CPU, memory, retained-disk and per-guest/shared caps stay separate by node; the
snapshot remains advisory and driver admission remains authoritative.

For requests without `--architecture`, the caller evaluates the primary source,
then additional sources in configured order, selecting the first source with a
current fit. It creates exactly one Nomad job using that source's original whole
catalog pin and native variant pins. The existing driver protocol is unchanged.
Nomad can place the job among currently fitting nodes for the selected source;
platform selection within that catalog remains late and durably bound by driver
events. Explicit architecture and repository-profile filters are also honored.
This does not require updating or restarting active Linux drivers just to share
a new aggregate catalog SHA.

The chosen source, reservation envelope and variants are saved before submission.
A lost submit response is lookup-only: new capacity or a new source cannot cause
another dispatch. A changed allocation attempt, selected pin, owner or bound
runtime configuration cannot rewrite an existing target config. Adding only the
optional discovery-source list permits lookup of a matching legacy request and
preserves its saved target bytes. Other configuration drift remains rejected.

Without `catalog_sources`, the existing single-catalog behavior and explicit
architecture contract remain unchanged. This feature is source-only until an
operator reviews and deploys the caller configuration. Keep primary pins and
existing per-session target configs intact during rollout. Do not publish a union
catalog to unchanged drivers or silently substitute a different release version.
