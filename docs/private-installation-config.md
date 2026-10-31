# Public source and private installation configuration

Installation endpoints, node names and identifiers, filesystem paths, namespaces,
storage buckets, identity references and network ranges belong in private
operator configuration outside the source checkout. Shipped examples contain
placeholders, reserved example domains and synthetic identities.

Configure Nomad, OpenBao, model and storage endpoints explicitly for each
installation. Keep credentials in protected provider/token files; pass references
through the supported configuration. Do not place credentials in connection
profiles, command arguments, job metadata, logs, fixtures or prepared images.

The public package contains generic driver/caller code and contracts. Private
deployment playbooks, installed inventories, operational observations and rollout
reports are excluded. Installing the package does not create identities, change
access grants, configure host networking or activate existing VM workloads.

Review the resulting private configuration and dependency versions before
installation. Source tests do not establish a live environment's readiness.
