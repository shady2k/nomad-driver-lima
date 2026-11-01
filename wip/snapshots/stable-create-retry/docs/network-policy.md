# Network policy boundary

No host firewall, VPN, system grants, LAN route or new network helper is installed
by this prototype. `plain` removes automatic mounts and forwarding conveniences; it
is not an outbound LAN ACL. Fingerprint explicitly reports network isolation false.

Official Lima network modes choose connectivity, not a portable destination/port
allowlist: https://lima-vm.io/docs/config/network/ . Default user-mode networking can
reach host/LAN destinations. Bridged/shared networking may need privileged upstream
helpers and does not itself provide a restrictive egress policy. Those are not enabled.

A production policy must be operator-owned and reviewed for the actual backend:
Linux QEMU user-mode sockets are host process connections; filtering only a guest
TAP/FORWARD chain does not cover them. macOS VZ and QEMU require separate verified
policy mechanisms. A guest-root firewall is not a security boundary against guest root.
No claim that Docker, no mounts, or a private VM subnet solves this issue is made.

Keep untrusted production admission disabled until a reviewed upstream/host policy
has both positive and negative tests. This document is planning only, not an applied
host policy or authorization to change one.

## Minimal proposed host boundary (not applied)

Use a dedicated unprivileged runtime account per node, rather than filtering the
operator's personal account. Run the unchanged Lima/QEMU/VZ processes under it via
`runtime_user`. Creating that account and granting existing KVM-group access on
Linux are separate operator-approved provisioning actions, not driver side effects.

For the current exact Lima 2.2.1 default network:

- QEMU/slirp creates outbound host sockets as the QEMU runtime owner.
- VZ starts an in-process gvisor usernet in its hostagent. The VZ PID file records
  that process PID. Mac acceptance confirmed DriverPID == HostAgentPID. This is not
  the separate optional `vzNAT` attachment and must not be generalized to it.
- Therefore Linux nftables OUTPUT `meta skuid` and macOS PF outbound TCP/UDP `user`
  matching are candidates for one node-wide policy without a proxy or new daemon.
  Packet/UID attribution and actual blocking still require negative live tests.

Upstream implementation references (pinned version):
https://github.com/lima-vm/lima/blob/v2.2.1/pkg/driver/vz/vm_darwin.go
and https://github.com/lima-vm/lima/blob/v2.2.1/pkg/store/instance.go .

Proposed policy order, expressed in operator configuration rather than project code:

1. Preserve only the exact runtime control connections needed for the managed SSH
   listeners and DNS resolver; preserve required established reply traffic. Do not
   allow all loopback traffic. Keep host runtime control Unix sockets private.
2. Permit individually approved private destination CIDR/port pairs (for example a
   model HTTPS endpoint). A shared ingress IP exception permits that IP's other
   applications on the allowed port; application authorization is still necessary.
3. Deny other loopback, LAN/private, link-local/cloud metadata, overlay/CGNAT and
   other non-global destinations in both IP families. DNS rebinding must still hit
   the IP destination deny. Test IPv4-mapped IPv6 and NAT64 paths, not just IPv4.
4. Allow the explicitly chosen public egress policy. Public HTTPS access is broader
   than a domain allowlist and must not be described as an equivalent replacement.

Lima auto-selects its local SSH TCP port. To avoid a broad loopback exception, the
next driver change can consume a native Nomad allocated port and pin Lima's
`ssh.localPort`; the host policy can allow only an operator-reserved loopback range.
The listener must remain loopback-only. No new scheduler or firewall-control service
is needed. This port integration is a proposal and has not been implemented.

One runtime UID shares one policy across its VMs. Host/guest-to-other-VM SSH access
inside an allowed control range is a residual surface; independent per-VM policy
requires a separately designed identity/interface boundary. Do not claim tenant
isolation merely from one node-wide UID rule.

On macOS, PF anchors/order, loopback skip rules, state handling and socket UID
attribution must be verified without replacing Apple's existing anchors. Read-only
inspection found no explicit `set skip on lo0` in the main host pf.conf, which does
not prove effective PF state or all loaded anchor behavior. Do not enable PF or
replace global rules as an incidental driver action. For a nonmatching network
backend (VZNAT/shared/bridged/user-v2), fail the isolation capability claim until its
own traffic path is verified. Guest root cannot be trusted to enforce these rules.

Required later approval is narrowly: dedicated runtime account, OS-managed persistent
nftables/PF anchor and exact operator destination/control-port exceptions, with an
out-of-band management path, rollback, positive and negative tests. No such account,
PF/nft rule, route, VPN, service, or production admission change was made here.
