# Public source and private installation configuration

Source-only metadata review 0.1.21 was based on 0.1.20 (1374445), not the separate
0.1.19 integration candidate (9164d4f). The minimal profile review 0.1.22 incorporates these metadata changes and adds
only repository-profile wiring; it excludes resume/resources. The metadata patch adds connection-info and descriptive
request provenance without driver/image/resume/profile/resource changes. Nothing
is installed and no current owner/job is rewritten. Merge these changes separately
when the integration candidate is selected; do not ship another package under an
already installed version number.

All installation URLs, host/node names, namespace inventory, filesystem paths,
storage bucket/pod names, existing identity names and cache/network ranges must be
provided by private installation vars/spec files. Examples use placeholders.
Credentials remain configured provider references; no real values enter source,
fixtures, command-rendering profiles or the VM image.

Changed deployment inputs (not applied):

| Helper | External fields |
|---|---|
| nomad-operator-cli | private `NOMAD_WORKERS_OPERATOR_CONFIG` JSON: bootstrap_file, nomad_address, nomad_binary; default config location under current HOME |
| nomad-vault-client | operator_bootstrap_file/address, node_id, runtime_home/user, vm, limactl, client_config/service, vault_config, backup_dir, nomad_binary |
| nomad-vault-server | vault_config |
| openbao-nomad-state | jwt_config.jwks_url and llm_data_path; exact generated single-record policy still checked |
| llm-record-state | explicit source/destination in same KV mount plus record_paths_approved; no field/grant expansion |
| nomad-node-name | exact old_name/new_name plus existing config checksum/approval guards |
| results-s3-access | bucket, bao_path, identity, policy, preserve_identities, storage_namespace/pod/container; IAM actions/prefix remain bounded |
| results-s3-caller | caller_home, exact reference, optional legacy_reader_config, installed exchange_module |
| final-worker-acceptance | `--spec` private file: caller_home/cli, catalog_config, operation_root, namespace, node_id, template_version, expected_provider/storage; explicit acceptance-only limits retained |
| storage-browser playbooks | storage_kubectl, storage_namespace, approved host, source ranges, storage_network_python |
| canary playbooks | operator_acceptance_root; acceptance caller playbook uses worker_acceptance_user/root |
| caller_verify | optional legacy_plugin_id from private catalog config; missing means preservation status unknown, not false success |

Deployment must map the existing reviewed private values before using a changed
helper. No helper is hot-replaced in this review, so existing configuration keeps
working. Changes never grant access or broaden a live rule. YAML parsing, Python
compilation and a mocked operator exec validate source behavior without Kubernetes,
OpenBao, SSH, credential extraction or model calls.

The previous installation-specific quickstart is retained in a private local audit
artifact outside this repository; the public quickstart now describes the generic
contract. A read-only history audit reports old commit IDs/categories separately.
A clean working tree is not clean history: no publication or history rewrite is
authorized or performed by this review.
