#!/usr/bin/env python3
"""Read-only source checks and private per-operation staging; no VM lifecycle."""
import hashlib,importlib.util,json,os,pathlib,pwd,re,shutil,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save as save
spec=importlib.util.spec_from_file_location('source_node',pathlib.Path(__file__).with_name('template-source-clone.py'));node=importlib.util.module_from_spec(spec);spec.loader.exec_module(node)
def preflight(packet,context,receipts=None):
 p=save.validate_operation(packet);s=p['source'];op=p['operation_id'];key=context['ledger_key']
 if save.sha(p)!=packet['plan_sha256'] or not re.fullmatch('[a-f0-9]{64}',key):raise RuntimeError('BOUND_OPERATION_AND_LEDGER_REQUIRED')
 state_path=pathlib.Path(context['state_dir'])/(key+'.json');state,digest=node.read_private(state_path)
 expected={'AllocID':s['allocation'],'TaskName':s['task'],'WorkerSessionID':s['session_id'],'CreatorAgentID':s['creator_agent_id'],'CreatorSessionID':s['creator_session_id']}
 if any(state.get(k)!=v for k,v in expected.items()):raise RuntimeError('SOURCE_PREFLIGHT_IDENTITY_OR_PHASE_CHANGED')
 if receipts and 'source_finish'in receipts:
  node.bound_clone(packet,receipts,context,lambda p,r,c:node.invoke(p,r,c,check_only=True))
 elif state.get('Phase')!='running':raise RuntimeError('SOURCE_PREFLIGHT_IDENTITY_OR_PHASE_CHANGED')
 user=pwd.getpwnam(context['runtime_user']);base=pathlib.Path(context['build_root']);root=base/op
 if not base.is_absolute() or base.is_symlink() or (root.exists() and root.is_symlink()):raise RuntimeError('PRIVATE_OPERATION_ROOT_REQUIRED')
 base.mkdir(parents=True,exist_ok=True,mode=0o700);root.mkdir(mode=0o700,exist_ok=True)
 if os.getuid()==0:os.chown(base,user.pw_uid,user.pw_gid);os.chown(root,user.pw_uid,user.pw_gid)
 if root.stat().st_uid!=user.pw_uid or root.stat().st_mode&0o077:raise RuntimeError('PRIVATE_OPERATION_ROOT_REQUIRED')
 marker={'operation_id':op,'plan_sha256':packet['plan_sha256'],'source_allocation':s['allocation']}
 def frozen(name,raw):
  dest=root/name
  if dest.is_symlink() or (dest.exists() and dest.read_bytes()!=raw):raise RuntimeError('STAGED_OPERATION_ASSET_CHANGED')
  if not dest.exists():
   fd=os.open(dest,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o444)
   with os.fdopen(fd,'wb')as f:f.write(raw);f.flush();os.fsync(f.fileno())
  if os.getuid()==0:os.chown(dest,user.pw_uid,user.pw_gid)
 frozen('prepare-operation.json',save.canonical(marker))
 deploy=pathlib.Path(__file__).parent;assets=deploy.parent/'plugin/guest'
 hashes={name:hashlib.sha256((assets/name).read_bytes()).hexdigest() for name in ('task-main.py','adapter.mjs','launcher.mjs')}
 frozen('guest-assets-frozen.json',save.canonical({'files':hashes}))
 for name in ('base-seal.sh','base-native-output-probe.py'):frozen(name,(deploy/name).read_bytes())
 definitions=pathlib.Path(context.get('model_definitions_file',str(deploy.parent/'plugin/tests/fixtures/provider-models.synthetic.json')))
 frozen('model-definitions.json',definitions.read_bytes())
 disk=int(context['disk_gib']);reserve=int(context.get('physical_reserve_gib',5));mem=int(context.get('acceptance_memory_mib',4096));cpu=int(context.get('acceptance_cpus',2))
 if disk<1 or disk>1024 or reserve<1 or not 1<=cpu<=8 or not 1024<=mem<=16384:raise RuntimeError('CANDIDATE_RESOURCE_BOUNDS')
 fs=os.statvfs(root);available=fs.f_bavail*fs.f_frsize
 # Conservative physical staging envelope: candidate + export + two boot disks.
 if available<(4*disk+reserve)*2**30:raise RuntimeError('INSUFFICIENT_PHYSICAL_TEMPLATE_WORKSPACE')
 if sys.platform=='linux':
  fields=dict(line.split(':',1) for line in pathlib.Path('/proc/meminfo').read_text().splitlines());free=int(fields['MemAvailable'].split()[0])*1024
  if free<(mem+1024)*2**20 or (os.cpu_count()or 0)<cpu:raise RuntimeError('INSUFFICIENT_NATIVE_ACCEPTANCE_RESOURCES')
 if node.read_private(state_path)[1]!=digest:raise RuntimeError('SOURCE_CHANGED_DURING_PREFLIGHT')
 return {'operation_id':op,'plan_sha256':packet['plan_sha256'],'source_node_current':True,'isolated_builder_capacity_available':True,'frozen_assets_staged':True,'build_root':str(root),'guest_assets':hashes}
if __name__=='__main__':
 try:
  context=exchange.protected(sys.argv[1]);payload=json.load(sys.stdin);print(json.dumps(preflight(payload['packet'],context,receipts=payload.get('receipts')if payload.get('stage')=='preflight_retained_source'else None)))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
