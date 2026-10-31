# Configure a caller

Build the driver with the command in the repository README. Install it in the
Nomad plugin directory configured by your operator. See `examples/client-plugin.hcl`
and `examples/job.nomad.hcl` for explicit, operator-owned paths and settings.

The `nomad-workers` caller is in `plugin/`. Copy a `plugin/config*.example.json`
file outside the source tree, replace every placeholder, and keep credentials in
private files referenced by that configuration. Nomad CLI authentication and
OpenBao/model/storage endpoints are operator-configured; no hosted service is
provided by this project. Never commit the resulting installation configuration.

Use synthetic identifiers for examples. Generate actual owner, agent and session
identities in the installation workflow, then preserve ownership boundaries.
Run the caller with an explicit config path:

```sh
python3 plugin/scripts/workerctl.py --config /absolute/private/caller.json --help
```

Run offline unit tests before installing a release. Live VM and credential-backed
acceptance requires a separate, explicitly configured environment.
