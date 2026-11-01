#!/usr/bin/env python3
"""Finite binding to the EXISTING base-artifact seal/export/boot/check/catalog.
Operator context supplies private node paths; agent inputs never select executables.
This binding does not stop sources, fetch credentials or publish objects.
"""
import ast,hashlib,importlib.util,json,os,pathlib,pwd,re,shutil,subprocess,sys,time
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save
DEPLOY=pathlib.Path(__file__).parent

def guest_program(packet,receipts,trusted_provenance=None):
 # Bundle only reviewed pure validation helpers and sanitizer in memory; no new
 # guest package/service and no writes outside a private /run marker + candidate.
 source=pathlib.Path(save.__file__).read_text();tree=ast.parse(source);functions='\n'.join(ast.get_source_segment(source,n) for n in tree.body if isinstance(n,ast.FunctionDef) and n.name in ('sha','validate_operation','composition_contract','verify_stage'))
 sanitizer=(DEPLOY/'template-sanitize.py').read_text();sanitizer=sanitizer[sanitizer.index('RUNTIME='):sanitizer.index("if __name__=='__main__':")]
 redactor=(DEPLOY.parent/'plugin/scripts/secure_logs.py').read_text();redactor=redactor[:redactor.index('class Spool:')]
 payload=json.dumps({'packet':packet,'receipts':receipts,'trusted_provenance':trusted_provenance})
 return '''import hashlib,json,os,pathlib,re,shutil,stat,types,uuid
'''+redactor+'''\ncanonical=lambda v:json.dumps(v,sort_keys=True,separators=(',',':')).encode()
'''+functions+'''\nsave=types.SimpleNamespace(sha=sha,composition_contract=composition_contract,verify_stage=verify_stage)
'''+sanitizer+'''
# Only this candidate may run the finite cleanup. No active Prime/Bao/acpx.
for proc in pathlib.Path('/proc').iterdir():
 if proc.name.isdigit():
  try:comm=(proc/'comm').read_text().strip()
  except (FileNotFoundError,PermissionError,ProcessLookupError):continue
  if comm in ('prime-agent','acpx','bao','vault'):raise RuntimeError('CANDIDATE_RUNTIME_PROCESS_PRESENT')
payload=json.loads('''+repr(payload)+''');p=payload['packet']['plan'];clone=payload['receipts']['clone_candidate']
marker={'operation_id':p['operation_id'],'plan_sha256':payload['packet']['plan_sha256'],'candidate_instance':clone['candidate_instance'],'source_allocation':p['source']['allocation'],'purpose':'candidate-only-sanitation'}
path='/run/nomad-workers-template-candidate.json'
fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'w') as f:json.dump(marker,f);f.flush();os.fsync(f.fileno())
print(json.dumps(scrub('/',payload['packet'],clone,marker,trusted_provenance=payload['trusted_provenance'])))
'''

def trusted_provenance(context):
 trusted=None
 if context.get('trusted_provenance_file')or context.get('trusted_provenance_sha256'):
  evidence_path=pathlib.Path(context['trusted_provenance_file'])
  if evidence_path.is_symlink()or not evidence_path.is_file()or evidence_path.stat().st_uid not in (0,os.geteuid())or evidence_path.stat().st_mode&0o022:raise RuntimeError('PRIVATE_TRUSTED_PROVENANCE_REQUIRED')
  evidence_raw=evidence_path.read_bytes()
  if hashlib.sha256(evidence_raw).hexdigest()!=context.get('trusted_provenance_sha256'):raise RuntimeError('TRUSTED_PROVENANCE_SHA_MISMATCH')
  trusted=json.loads(evidence_raw)
 return trusted

def run_stage(stage,packet,receipts,context,run=subprocess.run):
 if stage not in ('seal','export','boot','check','catalog'):raise RuntimeError('EXISTING_ARTIFACT_STAGE_REQUIRED')
 clone=save.verify_stage('clone_candidate',receipts['clone_candidate'],packet);candidate=clone.get('candidate_instance');p=packet['plan']
 if candidate!='agent-base-build-save-'+p['operation_id'].replace('-',''):raise RuntimeError('EXACT_CANDIDATE_REQUIRED')
 root=pathlib.Path(context['build_root'])/p['operation_id'];home=pathlib.Path(context['lima_home']);user=context['runtime_user'];uid=pwd.getpwnam(user).pw_uid
 if not root.is_dir() or root.is_symlink() or root.stat().st_uid!=uid or root.stat().st_mode&0o077:raise RuntimeError('PRIVATE_CANDIDATE_BUILD_ROOT_REQUIRED')
 lim=['/usr/sbin/runuser','-u',user,'--','/usr/bin/env','-i','HOME='+pwd.getpwnam(user).pw_dir,'PATH=/usr/local/bin:/usr/bin:/bin','LIMA_HOME='+str(home),context['limactl']]
 def call(args,input=None,timeout=2400):
  result=run(args,input=input,text=True,capture_output=True,timeout=timeout)
  if result.returncode:raise RuntimeError('CANDIDATE_STAGE_FAILED_DATA_RETAINED')
  return result.stdout
 def ready(name):
  deadline=time.monotonic()+min(600,context.get('readiness_timeout_seconds',600))
  while time.monotonic()<deadline:
   rows=run([*lim,'list','--json',name],text=True,capture_output=True,timeout=20)
   if rows.returncode==0:
    values=[json.loads(x) for x in rows.stdout.splitlines() if x]
    if len(values)==1 and values[0].get('name')==name and values[0].get('dir')==str(home/name) and values[0].get('status')=='Running':
     probe=run([*lim,'shell',name,'/usr/bin/true'],text=True,capture_output=True,timeout=20)
     if probe.returncode==0:return
   time.sleep(1)
  raise RuntimeError('CANDIDATE_BOOT_READINESS_TIMEOUT_RETAINED')
 if stage=='seal':
  trusted=trusted_provenance(context)
  # Never auto-retry after the controller's inflight journal. A failed sanitize
  # leaves candidate intact for inspection; no source operation is performed.
  rows=[json.loads(x) for x in call([*lim,'list','--json',candidate]).splitlines() if x]
  if len(rows)!=1 or rows[0].get('dir')!=str(home/candidate) or rows[0].get('status')!='Stopped':raise RuntimeError('CANDIDATE_NOT_EXACT_STOPPED_CLONE')
  call([*lim,'start','--tty=false',candidate]);ready(candidate)
  proof=json.loads(call([*lim,'shell',candidate,'sudo','/usr/bin/python3','-'],guest_program(packet,receipts,trusted)))
  if proof.get('composition_sha256')!=save.sha(p['data_composition']) or proof.get('runtime_credentials_absent')is not True:raise RuntimeError('CANDIDATE_SANITATION_PROOF_INCOMPLETE')
  save.atomic(root/'sanitation-receipt.json',proof)
 # Existing pipeline keeps its own operation lock, asset checks and QEMU/VZ
 # normal disk locks. Unique acceptance names avoid colliding with other saves.
 args=[context['python'],str(DEPLOY/'base-artifact.py'),stage,'--home',str(home),'--root',str(root),'--builder',candidate,'--limactl',context['limactl'],'--user',user,'--architecture',context['architecture'],'--vm-type',context['vm_type'],'--disk-gib',str(context['disk_gib']),'--template-id',p['template']['template_id'],'--version',p['template']['version'],'--acceptance-prefix','agent-base-accept-'+p['operation_id'].replace('-','')[:12],'--acceptance-cpus',str(context.get('acceptance_cpus',2)),'--acceptance-memory-mib',str(context.get('acceptance_memory_mib',4096))]
 if stage=='boot':
  probes=context.get('dependency_probes',[])
  expected=p['dependency_recipe']['packages']
  if not probes or sorted((v['manager'],v['name'],v['version']) for v in probes)!=sorted((v['manager'],v['name'],v['version']) for v in expected):raise RuntimeError('PINNED_DEPENDENCY_PROBES_REQUIRED')
  for index in (1,2):
   call([*args,'--boot-index',str(index)])
   acceptance='agent-base-accept-'+p['operation_id'].replace('-','')[:12]+'-'+str(index)
   ready(acceptance)
   observed=[]
   for probe in probes:
    argv=probe.get('argv')
    if not isinstance(argv,list) or not argv or any(not isinstance(v,str) or '\0' in v for v in argv) or not argv[0].startswith('/'):raise RuntimeError('FIXED_DEPENDENCY_PROBE_ARGV_REQUIRED')
    output=call([*lim,'shell',acceptance,*argv],timeout=60)
    if hashlib.sha256(output.encode()).hexdigest()!=probe.get('stdout_sha256'):raise RuntimeError('DEPENDENCY_VERSION_PROBE_MISMATCH')
    observed.append({k:probe[k] for k in ('manager','name','version','stdout_sha256')})
   save.atomic(root/('dependency-proof-'+str(index)+'.json'),observed)
   check=[*args];check[2]='check';call([*check,'--boot-index',str(index)])
   if index==2:
    definitions=json.loads((root/'model-definitions.json').read_text());models=definitions.get('models',definitions) if isinstance(definitions,dict) else definitions
    provider={'name':'gateway','base_url':'https://gateway.invalid/v1','model':'deepseek-v4.1-flash-auto','model_definitions':models}
    program=(DEPLOY/'base-model-catalog-probe.py').read_text()+'\nprobe('+repr(provider)+')\n'
    proof=json.loads(call([*lim,'shell',acceptance,'/usr/bin/python3','-'],program,timeout=240))
    proof.update(disk_sha256=json.loads((root/'artifact/export.json').read_text())['disk']['sha256'],guest_assets=json.loads((root/'seal-receipt.json').read_text())['guest_assets'])
    save.atomic(root/'artifact/model-catalog-proof.json',proof)
   stop=[*args];stop[2]='stop';call([*stop,'--boot-index',str(index)])
 elif stage!='check':
  if stage=='catalog':args+=['--model-catalog-proof',str(root/'artifact/model-catalog-proof.json')]
  call(args)
 out=root/'artifact';result={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256']}
 if stage=='seal':
  proof=json.loads((root/'sanitation-receipt.json').read_text());sealed=json.loads((root/'seal-receipt.json').read_text())
  if sealed.get('builder')!=candidate:raise RuntimeError('SEAL_RECEIPT_CANDIDATE_CHANGED')
  result.update(proof,identity_sealed=True,candidate_stopped=True)
 elif stage=='export':
  doc=json.loads((out/'export.json').read_text());result.update(standalone_disk=True,checksum_verified=True,disk_sha256=doc['disk']['sha256'])
 elif stage in ('boot','check'):
  if stage=='check':
   proof=json.loads((out/'acceptance.json').read_text())
   if proof.get('independent_boots')!=2 or not proof.get('unique_identities'):raise RuntimeError('ACCEPTANCE_INCOMPLETE')
   proofs=[json.loads((root/('dependency-proof-'+str(i)+'.json')).read_text()) for i in (1,2)]
   if not proofs[0] or proofs[0]!=proofs[1]:raise RuntimeError('DEPENDENCY_BOOT_PROOFS_DIFFER')
   result.update(unique_identities=True,dependency_versions_verified=True,base_checksum_unchanged=True)
  else:result['two_independent_boots']=True
 else:
  entry=json.loads((out/'catalog.json').read_text())['templates'][0]
  if entry['template_id']!=p['template']['template_id'] or entry['version']!=p['template']['version']:raise RuntimeError('CATALOG_ENTRY_CHANGED')
  result['new_version_no_overwrite']=True
 return save.verify_stage(stage,result,packet)
if __name__=='__main__':
 try:
  import exchange
  context=exchange.protected(sys.argv[1]);payload=json.load(sys.stdin);print(json.dumps(run_stage(payload['stage'],payload['packet'],payload['receipts'],context)))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
