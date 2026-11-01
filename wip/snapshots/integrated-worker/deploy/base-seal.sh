#!/bin/bash
# Run only inside the isolated, never-credentialed clean builder, after readiness.
set -euo pipefail
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
stage() { printf '{"timestamp":"%s","stage":"%s"}\n' "$(date -u +%FT%TZ)" "$1"; }
test -s /opt/worker/.agent-base-v1
test "$(stat -c %a /tmp)" = 1777
for asset in task-main.py adapter.mjs launcher.mjs; do test -r "/opt/nomad-workers/$asset"; done
stage seal_readiness
/usr/local/libexec/agent-base-check
# The clean builder has never received provider or OpenBao credentials. Refuse
# known application state rather than trying to sanitize a credentialed worker.
for path in /root/.prime /root/.acpx /root/.bao-token /root/.vault-token /root/.config/prime /opt/worker/secrets /opt/worker/state; do
  if test -e "$path"; then echo "Unexpected application state: $path" >&2; exit 65; fi
done
if pgrep -x prime-agent >/dev/null || pgrep -x acpx >/dev/null; then echo 'Runtime process survived readiness' >&2; exit 65; fi
stage seal_identity
# Stop persistent journal writes using systemd's supported handoff to /run,
# then remove this builder's persistent journal and diagnostic logs only.
journalctl --relinquish-var
find /var/log/journal -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
find /var/log -xdev -type f -exec truncate -s 0 -- {} +
# cloud-init's documented golden-image operation reruns native first-boot setup.
cloud-init clean --logs --machine-id --configs all --seed
systemctl stop systemd-random-seed.service
rm -f /var/lib/systemd/random-seed /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
# Only generated builder access material and diagnostic histories are removed.
find /root /home -xdev -type f \( -name authorized_keys -o -name known_hosts -o -name .bash_history \) -delete
rm -f /var/lib/dhcp/*.leases /var/lib/dhcp/*.leases~
if test -e /var/lib/dbus/machine-id && ! test -L /var/lib/dbus/machine-id; then rm -f /var/lib/dbus/machine-id; ln -s /etc/machine-id /var/lib/dbus/machine-id; fi
test "$(cat /etc/machine-id)" = uninitialized
# Do not preserve the builder's hostname as a unique machine identity.
old_hostname=$(cat /etc/hostname)
python3 - "$old_hostname" <<'HOSTS'
import pathlib,sys
p=pathlib.Path('/etc/hosts');p.write_text(p.read_text().replace(sys.argv[1].strip(),'agent-base'))
HOSTS
printf 'agent-base\n' > /etc/hostname
sync
fstrim -v /
stage seal_complete_poweroff_required
