#!/usr/bin/env python3
"""One-shot maintenance controller. Existing SSH to each host; never a service.

The operator host performs the real Nomad GET and filters its output there.
No Nomad credential, allocation job spec, task environment or secret crosses to
this controller or the runtime host. Trust is the existing authenticated SSH
controller, not a nonce-based attestation claim.
"""
import argparse
import datetime as dt
import json
import os
import re
import selectors
import shlex
import subprocess
import sys
import time

# Executed only on the existing operator host, under its existing authorization.
OPERATOR_READ = r'''
import datetime as dt,json,subprocess,sys
try:
 p=subprocess.run([sys.argv[1],"operator","api","-namespace="+sys.argv[2],"-X","GET","/v1/allocation/"+sys.argv[3]],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5,check=False)
 if p.returncode or len(p.stdout)>2*1024*1024: raise ValueError()
 a=json.loads(p.stdout)
 result={k:a.get(k) for k in ["ID","NodeID","JobID","Namespace","ClientStatus","NextAllocation","ModifyIndex"]}
 if result["NextAllocation"] is None:result["NextAllocation"]=""
 states=a.get("TaskStates")
 if not isinstance(states,dict) or len(states)>32:raise ValueError()
 result["TaskStates"]={}
 for name,t in states.items():
  if not isinstance(t,dict):raise ValueError()
  events=t.get("Events") or []
  if len(events)>1024:raise ValueError()
  result["TaskStates"][name]={k:t.get(k) for k in ["State","Failed","Restarts","StartedAt","LastRestart"]}
  result["TaskStates"][name]["Events"]=[{"Type":e.get("Type")} for e in events]
 answer={"allocation":result,"observed_at":dt.datetime.now(dt.timezone.utc).isoformat()}
 data=json.dumps(answer,separators=(",",":"))
 if len(data)>60000:raise ValueError()
 print(data)
except Exception:
 sys.stderr.write("operator allocation verification failed\n")
 sys.exit(1)
'''

# Status-aware GC reader executes only on the already authorized operator host.
# It consumes that host's existing protected auth reference locally; no token
# value is sent through SSH, stdout, argv, logs or to the runtime/controller.
OPERATOR_READ_GC = r'''
import datetime as dt,json,os,stat,sys,urllib.parse,urllib.request,urllib.error
try:
 base,token_path,token_format,allocation,namespace,node=sys.argv[1:]
 u=urllib.parse.urlsplit(base)
 if u.scheme!="https" or not u.hostname or u.username or u.password or u.path or u.query or u.fragment:raise ValueError()
 fd=os.open(token_path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o600:raise ValueError()
  raw=f.read(65537)
 if len(raw)>65536:raise ValueError()
 token=json.loads(raw)["SecretID"] if token_format=="json" else raw.strip()
 if not isinstance(token,str) or not token:raise ValueError()
 class NoRedirect(urllib.request.HTTPRedirectHandler):
  def redirect_request(self,*a,**kw):return None
 opener=urllib.request.build_opener(NoRedirect())
 def get(path):
  try:
   with opener.open(urllib.request.Request(base+path,headers={"X-Nomad-Token":token}),timeout=2) as r:
    body=r.read(2*1024*1024+1)
    if len(body)>2*1024*1024:raise ValueError()
    return r.status,json.loads(body)
  except urllib.error.HTTPError as e:
   if e.code==404:return 404,None
   raise ValueError("authorization/HTTP failure") from None
 identity_status,identity=get("/v1/acl/token/self")
 if identity_status!=200 or identity.get("Type")!="management":raise ValueError("full-node proof needs existing management identity")
 q="?namespace="+urllib.parse.quote(namespace,safe="")
 status,a=get("/v1/allocation/"+allocation+q)
 if status==404:
  ns,n=get("/v1/node/"+node)
  als,rows=get("/v1/node/"+node+"/allocations?namespace=*")
  if ns!=200 or als!=200 or n.get("ID")!=node or not isinstance(rows,list) or any(v.get("ID")==allocation for v in rows):raise ValueError()
  result={"OperatorManagementVerified":True,"HTTPStatus":404,"NodeHTTPStatus":200,"NodeAllocationsHTTPStatus":200,"VerifiedNodeID":node,"NodeActiveAllocations":sum(v.get("ClientStatus") not in ("complete","failed","lost") for v in rows)}
 else:
  if status!=200:raise ValueError()
  result={k:a.get(k) for k in ["ID","NodeID","JobID","Namespace","ClientStatus","NextAllocation","ModifyIndex"]}
  result["HTTPStatus"]=200
  if result["NextAllocation"] is None:result["NextAllocation"]=""
  states=a.get("TaskStates")
  if not isinstance(states,dict) or len(states)>32:raise ValueError()
  result["TaskStates"]={}
  for name,t in states.items():
   events=t.get("Events") or []
   if len(events)>1024:raise ValueError()
   result["TaskStates"][name]={k:t.get(k) for k in ["State","Failed","Restarts","StartedAt","LastRestart"]}
   result["TaskStates"][name]["Events"]=[{"Type":v.get("Type")} for v in events]
 answer=json.dumps({"allocation":result,"observed_at":dt.datetime.now(dt.timezone.utc).isoformat()},separators=(",",":"))
 if len(answer)>60000:raise ValueError()
 print(answer)
except Exception:
 sys.stderr.write("operator status-aware verification failed\n");sys.exit(1)
'''

class RecoveryError(Exception):
    pass


def ssh_prefix(args):
    # Existing known_hosts and authentication only. No forwarding or acceptance.
    cmd = ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ForwardAgent=no",
           "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5"]
    if args.ssh_config:
        cmd += ["-F", args.ssh_config]
    if args.ssh_identity:
        cmd += ["-i", args.ssh_identity, "-o", "IdentitiesOnly=yes"]
    return cmd


def target_from(args):
    target={"key": args.key, "allocation_id": args.allocation,
            "node_id": args.node, "job_id": args.job, "namespace": args.namespace}
    if getattr(args,"operation","recover-absent-create")=="purge-retained":
        target["instance"]=args.instance
        if getattr(args,"ledger_sha256",None):target["ledger_sha256"]=args.ledger_sha256
        if getattr(args,"allow_gc_404",False):target["allow_gc_404"]=True
    return target


def challenge_valid(request, target, sequence, seen):
    if (set(request) != {"version", "type", "sequence", "nonce", "target", "deadline"}
            or request["version"] != 1 or request["type"] != "allocation_check"
            or request["sequence"] != sequence or request["target"] != target
            or not isinstance(request["nonce"], str)
            or not re.fullmatch(r"[a-f0-9]{64}", request["nonce"])
            or request["nonce"] in seen):
        raise RecoveryError("runtime verification challenge mismatched/replayed")
    deadline = dt.datetime.fromisoformat(request["deadline"].replace("Z", "+00:00"))
    remaining = (deadline - dt.datetime.now(dt.timezone.utc)).total_seconds()
    if not 0 < remaining <= 10:
        raise RecoveryError("runtime verification challenge expired or clock skewed")
    seen.add(request["nonce"])


def operator_read(args):
    command = [args.operator_python, "-c", OPERATOR_READ, args.operator_cli,
               args.namespace, args.allocation]
    if getattr(args,"allow_gc_404",False):
        if not args.operator_token_file or not args.operator_address or not args.ledger_sha256:raise RecoveryError("GC status verifier needs existing operator auth reference and exact ledger SHA")
        command=[args.operator_python,"-c",OPERATOR_READ_GC,args.operator_address,args.operator_token_file,args.operator_token_format,args.allocation,args.namespace,args.node]
    r = subprocess.run(ssh_prefix(args) + [args.operator_host, shlex.join(command)],
                       stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                       stderr=subprocess.DEVNULL, timeout=7 if getattr(args,"allow_gc_404",False) else 6, check=False)
    if r.returncode or len(r.stdout) > 60000:
        raise RecoveryError("fresh operator GET failed; no credential fallback")
    observation = json.loads(r.stdout)
    if set(observation) != {"allocation", "observed_at"}:
        raise RecoveryError("operator status schema invalid")
    return observation


class Lines:
    def __init__(self, stream):
        self.stream = stream
        self.selector = selectors.DefaultSelector()
        self.selector.register(stream, selectors.EVENT_READ)
        self.pending = b""

    def read(self, deadline):
        while b"\n" not in self.pending:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not self.selector.select(remaining):
                raise RecoveryError("runtime protocol timed out; outcome unknown")
            chunk = os.read(self.stream.fileno(), 8192)
            if not chunk:
                raise RecoveryError("runtime disconnected; outcome unknown")
            self.pending += chunk
            if len(self.pending) > 65536:
                raise RecoveryError("runtime response oversized; outcome unknown")
        line, self.pending = self.pending.split(b"\n", 1)
        return json.loads(line)

    def close(self):
        self.selector.close()


def runtime_command(args):
    command = [args.driver, getattr(args,"operation","recover-absent-create"), "--operator-stdio",
               "--state-dir", args.state_dir, "--lima-home", args.lima_home,
               "--runtime-user", args.runtime_user, "--limactl", args.limactl,
               "--key", args.key, "--allocation", args.allocation,
               "--node", args.node, "--job", args.job,
               "--namespace", args.namespace, "--failure-log", args.failure_log]
    if getattr(args,"operation","recover-absent-create")=="purge-retained":
        if not args.confirm_purge or not args.instance:raise RecoveryError("exact instance and --confirm-purge required")
        command += ["--instance",args.instance,"--confirm-purge"]
        if getattr(args,"ledger_sha256",None):command += ["--ledger-sha256",args.ledger_sha256]
        if getattr(args,"allow_gc_404",False):command += ["--allow-gc-404"]
    if args.runtime_sudo:
        command = ["sudo", "-n", "--"] + command
    return ssh_prefix(args) + [args.runtime_host, shlex.join(command)]


def run(args):
    process = subprocess.Popen(runtime_command(args), stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    lines = Lines(process.stdout)
    deadline = time.monotonic() + 35
    seen = set()
    target = target_from(args)
    try:
        message = lines.read(deadline)
        if "type" not in message:
            # A completed earlier recovery returns its durable audit idempotently.
            if not message.get("already_recovered"):
                raise RecoveryError("missing runtime verification; outcome unknown")
        else:
            for sequence in (1, 2):
                challenge_valid(message, target, sequence, seen)
                observation = operator_read(args)  # A new real GET, each time.
                reply = {"version": 1, "type": "allocation_observation",
                         "sequence": sequence, "nonce": message["nonce"],
                         "target": target, **observation}
                process.stdin.write(json.dumps(reply, separators=(",", ":")).encode() + b"\n")
                process.stdin.flush()
                message = lines.read(deadline)
        if (set(message)!={"key","instance","phase","audit_sha256","already_recovered"}
                or not isinstance(message.get("instance"),str)
                or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,55}",message["instance"])
                or message.get("key") != args.key or message.get("phase") != ("destroyed_disk_purged" if getattr(args,"operation","recover-absent-create")=="purge-retained" else "no_vm_failed")
                or not re.fullmatch(r"[a-f0-9]{64}", message.get("audit_sha256", ""))):
            raise RecoveryError("runtime recovery result invalid; outcome unknown")
        process.stdin.close()
        if process.wait(timeout=max(0.1, deadline - time.monotonic())) != 0:
            raise RecoveryError("runtime completion unconfirmed; outcome unknown")
        print(json.dumps(message, separators=(",", ":")))
    finally:
        lines.close()
        process.stdout.close()
        if not process.stdin.closed:
            process.stdin.close()
        if process.poll() is None:
            # Close only this SSH client. The bounded remote maintenance command
            # fails closed on EOF; if commit already occurred, outcome is unknown.
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)


def parse_args():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("runtime-host", "operator-host", "driver", "state-dir", "lima-home",
                 "runtime-user", "limactl", "key", "allocation", "node",
                 "namespace", "operator-python"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--job",default="")
    p.add_argument("--operator-cli",default="")
    p.add_argument("--operator-token-file")
    p.add_argument("--operator-token-format",choices=["json","plain"],default="json")
    p.add_argument("--operator-address")
    p.add_argument("--ledger-sha256")
    p.add_argument("--allow-gc-404",action="store_true")
    p.add_argument("--failure-log",default="")
    p.add_argument("--operation",choices=["recover-absent-create","purge-retained"],default="recover-absent-create")
    p.add_argument("--instance")
    p.add_argument("--confirm-purge",action="store_true")
    p.add_argument("--ssh-config")
    p.add_argument("--ssh-identity")
    p.add_argument("--runtime-sudo", action="store_true")
    return p.parse_args()


if __name__ == "__main__":
    try:
        run(parse_args())
    except Exception:
        # Never echo remote raw payload, SSH stderr, task env or credentials.
        sys.stderr.write("recovery verification/completion failed or unknown; retain intent, inspect same target, do not create/replay\n")
        sys.exit(1)
