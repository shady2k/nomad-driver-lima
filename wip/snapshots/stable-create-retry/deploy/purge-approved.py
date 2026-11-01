"""One-shot addressed maintenance. Stop at the first failed/unknown outcome."""
import argparse,hashlib,json,pathlib,subprocess,sys,datetime
p=argparse.ArgumentParser();p.add_argument('--spec',required=True);a=p.parse_args();s=json.loads(pathlib.Path(a.spec).read_text())
manifest=pathlib.Path(s['manifest']);assert hashlib.sha256(manifest.read_bytes()).hexdigest()==s['manifest_sha256'];m=json.loads(manifest.read_text());targets=m['targets'];assert len(targets)==5
assert {(t['key'],t['instance']) for t in targets}=={(t['key'],t['instance']) for t in s['approved_targets']}
controller=pathlib.Path(s['controller']);assert hashlib.sha256(controller.read_bytes()).hexdigest()==s['controller_sha256']
root=pathlib.Path(s['receipt_dir']);root.mkdir(parents=True,exist_ok=True,mode=0o700)
def save(p,value):
 import os,tempfile
 fd,tmp=tempfile.mkstemp(prefix='.purge-',dir=p.parent)
 with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),0o600);json.dump(value,f);f.flush();os.fsync(f.fileno())
 pathlib.Path(tmp).replace(p)
for index,t in enumerate(targets,1):
 assert t['instance_dir']==m['lima_home']+'/'+t['instance']
 intent=root/(t['key']+'.started.json');receipt=root/(t['key']+'.receipt.json')
 if intent.exists() or receipt.exists():raise SystemExit('PRIOR_OUTCOME_MUST_BE_INSPECTED_SAME_TARGET_BEFORE_RETRY')
 save(intent,{'key':t['key'],'instance':t['instance'],'ledger_sha256':t['ledger_sha256'],'time':datetime.datetime.now(datetime.timezone.utc).isoformat()})
 cmd=[s['python'],str(controller),'--operation','purge-retained','--confirm-purge','--allow-gc-404','--instance',t['instance'],'--key',t['key'],'--ledger-sha256',t['ledger_sha256'],'--allocation',t['allocation'],'--node',m['node_id'],'--namespace',t['historical_reference']['namespace'],'--runtime-host',s['runtime_host'],'--operator-host',s['operator_host'],'--ssh-identity',s['ssh_identity'],'--ssh-config',s['ssh_config'],'--runtime-sudo','--driver',s['remote_binary'],'--state-dir',m['state_dir'],'--lima-home',m['lima_home'],'--runtime-user',m['runtime_user'],'--limactl',m['limactl'],'--operator-python','python3','--operator-address',s['operator_address'],'--operator-token-file',s['operator_token_file'],'--operator-token-format','json']
 if t['historical_reference'].get('job'):cmd+=['--job',t['historical_reference']['job']]
 r=subprocess.run(cmd,capture_output=True,text=True,timeout=45)
 if r.returncode:
  save(root/(t['key']+'.unknown.json'),{'key':t['key'],'instance':t['instance'],'exit':r.returncode,'time':datetime.datetime.now(datetime.timezone.utc).isoformat()});print(json.dumps({'stage':'purge_failed_or_unknown_stop','index':index,'instance':t['instance'],'key':t['key'],'later_targets_not_attempted':True}),flush=True);raise SystemExit(1)
 v=json.loads(r.stdout);assert v['key']==t['key'] and v['instance']==t['instance'] and v['phase']=='destroyed_disk_purged';save(receipt,v);print(json.dumps({'stage':'addressed_purge_confirmed','index':index,**v}),flush=True)
print(json.dumps({'stage':'five_addressed_purges_confirmed','targets':5,'quota_release_expected_gib':100,'base_or_service_paths_deleted':False}),flush=True)
