#!/usr/bin/env python3
"""Read-only probe; run via an existing approved SSH connection, no sudo implicit."""
import argparse, json, os, pwd, shutil, subprocess

def run(argv):
    try:
        p = subprocess.run(argv, capture_output=True, text=True, timeout=10)
        return {"ok": p.returncode == 0, "code": p.returncode,
                "output": p.stdout.strip() if p.returncode == 0 else "unavailable"}
    except (OSError, subprocess.TimeoutExpired) as e:
        return {"ok": False, "error": type(e).__name__}

p = argparse.ArgumentParser()
p.add_argument('--runtime-user', required=True)
p.add_argument('--limactl', required=True)
p.add_argument('--nomad', required=True)
p.add_argument('--lima-home', required=True)
p.add_argument('--service', required=True)
a = p.parse_args()
u = pwd.getpwnam(a.runtime_user)
r = {"runtime": {"uid": u.pw_uid, "gid": u.pw_gid,
                 "groups": os.getgrouplist(a.runtime_user, u.pw_gid)},
     "cpu_count": os.cpu_count(),
     "disk_bytes": dict(zip(('total','used','free'), shutil.disk_usage(a.lima_home))),
     "lima": run([a.limactl, '--version']),
     "nomad_version": run([a.nomad, 'version']),
     "service": run(['systemctl','show',a.service,'--no-pager',
                     '--property=User,Group,FragmentPath,ActiveState']),
     "node": run([a.nomad,'node','status','-self','-json']),
     "allocations_table": run([a.nomad,'node','status','-self']),
     "empty_node_confirmed": False}
# KVM access check is meaningful only when invoked as the selected runtime user.
r['kvm_access_as_runtime'] = (os.access('/dev/kvm', os.R_OK | os.W_OK)
                              if os.geteuid() == u.pw_uid else 'unknown')
print(json.dumps(r, indent=2))
