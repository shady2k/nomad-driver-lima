#!/usr/bin/env python3
"""Stopped native QEMU/VZ artifact export and two fresh Lima boot acceptance; no production changes."""
import argparse,fcntl,hashlib,json,os,pathlib,pwd,subprocess,time,sys
from datetime import datetime,timezone
from base_platform import Platform,raw_gpt_info,export_raw,validate_boot_proofs,model_catalog_capability
p=argparse.ArgumentParser();p.add_argument('phase',choices=['seal','export','boot','check','catalog','stop']);p.add_argument('--home',required=True);p.add_argument('--root',required=True);p.add_argument('--builder',required=True);p.add_argument('--limactl',required=True);p.add_argument('--user',required=True);p.add_argument('--version');p.add_argument('--template-id',default='agent-base');p.add_argument('--acceptance-prefix',default='agent-base-accept');p.add_argument('--boot-index',type=int,choices=[1,2]);p.add_argument('--acceptance-cpus',type=int,default=2);p.add_argument('--acceptance-memory-mib',type=int,default=4096);p.add_argument('--architecture',choices=['amd64','arm64'],default='amd64');p.add_argument('--vm-type',choices=['qemu','vz'],default='qemu');p.add_argument('--disk-gib',type=int,default=8);p.add_argument('--model-catalog-proof');a=p.parse_args()
assert __import__('re').fullmatch('agent-base-accept(?:-[a-f0-9]{12})?',a.acceptance_prefix)
platform=Platform(a.architecture,a.vm_type);platform.require_host()
assert 1<=a.acceptance_cpus<=8 and 1024<=a.acceptance_memory_mib<=16384
if a.vm_type=='vz' and a.phase in ('boot','check','stop'):assert a.boot_index is not None,'VZ acceptance requires explicit sequential boot index'
indices=[a.boot_index] if a.boot_index else [1,2]
root=pathlib.Path(a.root);home=pathlib.Path(a.home)
assert root.is_absolute() and home.is_absolute() and a.builder.startswith('agent-base-build-')
assert root.owner()==a.user and home.owner()==a.user
lock_fd=os.open(root/'artifact-operation.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
fcntl.flock(lock_fd,fcntl.LOCK_EX)
out=root/'artifact';out.mkdir(mode=0o700,exist_ok=True)
if os.getuid()==0:os.chown(out,pwd.getpwnam(a.user).pw_uid,pwd.getpwnam(a.user).pw_gid)
env=['/usr/bin/env','-i','HOME='+pwd.getpwnam(a.user).pw_dir,'PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin','LIMA_HOME='+str(home)]
lim=([('/usr/bin/sudo' if sys.platform=='darwin' else '/usr/sbin/runuser'),'-u',a.user,'--',*env,a.limactl] if os.getuid()==0 else [*env,a.limactl])
def stage(stage_name,**kw):print(json.dumps({'timestamp':datetime.now(timezone.utc).isoformat(),'stage':stage_name,**kw}),flush=True)
def run(argv,timeout=120):return subprocess.check_output(argv,stderr=subprocess.STDOUT,text=True,timeout=timeout)
def sha(path):
 h=hashlib.sha256()
 with path.open('rb') as f:
  for b in iter(lambda:f.read(8*1024*1024),b''):h.update(b)
 return h.hexdigest()
def save(path,v):
 tmp=path.with_suffix('.tmp');tmp.write_text(json.dumps(v,indent=2)+'\n');tmp.replace(path)
def stopped_raw_guard(name):
 instance=home/name
 assert not instance.is_symlink() and instance.is_dir()
 observed=run([*lim,'list','--format','{{.Name}}|{{.Status}}|{{.Dir}}',name]).strip()
 assert observed==name+'|Stopped|'+str(instance),'Exact VZ instance must be stopped'
 import re
 config=(instance/'lima.yaml').read_text()
 assert re.search(r'^vmType: vz\s*$',config,re.M) and re.search(r'^arch: aarch64\s*$',config,re.M),'Wrong native VZ platform'
 for pidfile in instance.glob('*.pid'):
  text=pidfile.read_text().strip();assert text.isdigit(),'Uncertain PID metadata'
  try:os.kill(int(text),0)
  except ProcessLookupError:pass
  else:raise RuntimeError('Instance process still exists')
 r=subprocess.run(['/usr/sbin/lsof','-t','--',str(instance/'disk')],capture_output=True,text=True,timeout=20)
 assert r.returncode in (0,1) and not r.stderr.strip(),'lsof check unavailable'
 open_pids=r.stdout.split();assert all(x.isdigit() and int(x)==os.getpid()for x in open_pids),'Disk open by another process'
 assert (r.returncode==0)==bool(open_pids),'Uncertain lsof outcome'
def check_stopped_disk(name):
 disk=home/name/'disk'
 if platform.vm_type=='vz':
  stopped_raw_guard(name);return raw_gpt_info(disk,a.disk_gib*2**30)
 run(['qemu-img','check',str(disk)],timeout=600)
 return json.loads(run(['qemu-img','info','--output=json',str(disk)]))

if a.phase=='seal':
 if (root/'seal-receipt.json').exists():stage('sealed_receipt_already_present');sys.exit(0)
 expected=json.loads((root/'guest-assets-frozen.json').read_text())
 if 'files' in expected:expected={pathlib.Path(k).name:v for k,v in expected['files'].items()}
 assert set(expected)=={'task-main.py','adapter.mjs','launcher.mjs'}
 got=run([*lim,'shell',a.builder,'sudo','sha256sum',*['/opt/nomad-workers/'+name for name in expected]])
 observed={pathlib.Path(line.split()[1]).name:line.split()[0] for line in got.splitlines()}
 assert observed==expected,'frozen guest assets differ; refresh before seal'
 script=(root/'base-seal.sh').read_text()
 stage('seal_started')
 r=subprocess.run([*lim,'shell',a.builder,'sudo','/bin/bash','-s'],input=script,text=True,capture_output=True,timeout=300)
 (root/'seal.log').write_text(r.stdout+r.stderr)
 assert r.returncode==0,'seal failed; inspect seal.log; builder retained'
 stage('seal_completed_stopping_builder')
 run([*lim,'stop','--tty=false',a.builder],timeout=240)
 # A normal write-locking check also refuses if a surviving QEMU still owns disk.
 check_stopped_disk(a.builder)
 save(root/'seal-receipt.json',{'builder':a.builder,'sealed_at':datetime.now(timezone.utc).isoformat(),'method':'cloud-init clean --logs --machine-id --configs all --seed; regenerated SSH keys/random seed on fresh boot','seal_log_sha256':sha(root/'seal.log'),'guest_assets':expected,'architecture':a.architecture,'vm_type':a.vm_type})
 stage('sealed_builder_stopped')
elif a.phase=='export':
 assert (root/'seal-receipt.json').exists(),'explicit completed seal receipt required'
 sealed=json.loads((root/'seal-receipt.json').read_text());assert sealed.get('architecture','amd64')==a.architecture and sealed.get('vm_type','qemu')==a.vm_type,'Wrong sealed platform'
 disk=home/a.builder/'disk';assert disk.is_file() and not disk.is_symlink()
 # Never use -U or bypass QEMU's normal exclusive image lock.
 info=check_stopped_disk(a.builder);assert info['format']==platform.format
 dest=out/platform.filename
 if dest.exists():
  previous=json.loads((out/'export.json').read_text());assert sha(dest)==previous['disk']['sha256'];stage('immutable_export_already_verified');sys.exit(0)
 stage('disk_convert_started',virtual_size_bytes=info['virtual-size'])
 start=time.monotonic()
 if platform.vm_type=='vz':result=export_raw(disk,dest,info['virtual-size'],lambda:stopped_raw_guard(a.builder))
 else:
  run(['qemu-img','convert','-O','qcow2','-c','-o','compat=1.1,compression_type=zlib',str(disk),str(dest)],timeout=1800)
  run(['qemu-img','check',str(dest)],timeout=600)
  result=json.loads(run(['qemu-img','info','--output=json',str(dest)]))
 assert not result.get('backing-filename') and result['virtual-size']==info['virtual-size']
 os.chmod(dest,0o444)
 receipt={'disk':{'sha256':sha(dest),'size_bytes':dest.stat().st_size,'virtual_size_bytes':result['virtual-size'],'minimum_disk_gib':(result['virtual-size']+2**30-1)//2**30,'format':platform.format},'duration_seconds':time.monotonic()-start,'source':str(disk),'architecture':a.architecture,'vm_type':a.vm_type,'timestamp':datetime.now(timezone.utc).isoformat()}
 save(out/'export.json',receipt);stage('disk_exported',**receipt)
elif a.phase=='boot':
 receipt=json.loads((out/'export.json').read_text());assert receipt.get('architecture','amd64')==a.architecture and receipt.get('vm_type','qemu')==a.vm_type;disk=out/platform.filename;assert sha(disk)==receipt['disk']['sha256']
 # Runtime YAML intentionally contains no provision/user/mount/provider configuration.
 runtime=platform.runtime_yaml()
 (out/'runtime.yaml').write_text(runtime)
 for index in indices:
  name=a.acceptance_prefix+'-'+str(index);instance=home/name
  config=root/(name+'.yaml')
  if platform.vm_type=='vz':
   other=a.acceptance_prefix+'-'+str(3-index)
   if (home/other).exists():stopped_raw_guard(other)
  config.write_text(runtime+'images:\n- location: '+str(disk)+'\n  arch: '+platform.lima_arch+'\n  digest: sha256:'+receipt['disk']['sha256']+'\ncpus: '+str(a.acceptance_cpus)+'\nmemory: '+str(a.acceptance_memory_mib)+'MiB\ndisk: '+str(receipt['disk']['minimum_disk_gib'])+'GiB\n')
  ownership={'name':name,'artifact_root':str(root),'disk_sha256':receipt['disk']['sha256']}
  if not instance.exists():
   run([*lim,'create','--tty=false','--name='+name,str(config)],timeout=600)
   if a.acceptance_prefix!='agent-base-accept':save(instance/'template-acceptance-owner.json',ownership)
  elif a.acceptance_prefix!='agent-base-accept':
   marker=instance/'template-acceptance-owner.json'
   assert not marker.is_symlink()and marker.is_file()and json.loads(marker.read_text())==ownership,'Existing acceptance VM belongs to another operation; do not start'
  if (root/(name+'-start.json')).exists():stage('acceptance_boot_previously_launched',name=name);continue
  log=(root/(name+'-start.log')).open('x');proc=subprocess.Popen([*lim,'start','--tty=false','--progress','--timeout=10m',name],stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
  save(root/(name+'-start.json'),{'pid':proc.pid,'started_at':datetime.now(timezone.utc).isoformat(),'name':name});stage('acceptance_boot_started',name=name,pid=proc.pid)
elif a.phase=='check':
 receipt=json.loads((out/'export.json').read_text());assert sha(out/platform.filename)==receipt['disk']['sha256']
 checks=[]
 script="""import json,pathlib,subprocess,time
assert pathlib.Path('/workspace/repo').stat().st_uid==__import__('os').getuid() and __import__('os').access('/workspace/repo',__import__('os').W_OK),'workspace not writable by guest'
r=subprocess.run(['/usr/local/libexec/agent-base-check'],capture_output=True,text=True,timeout=180)
assert r.returncode==0,r.stderr[-1000:]
st=__import__('os').statvfs('/');rootfs={'total_bytes':st.f_blocks*st.f_frsize,'available_bytes':st.f_bavail*st.f_frsize,'allocated_bytes':(st.f_blocks-st.f_bfree)*st.f_frsize};partitions=json.loads(subprocess.check_output(['lsblk','--json','-b','-o','NAME,SIZE,TYPE,FSTYPE,MOUNTPOINTS'],text=True))
print(json.dumps({'root_filesystem':rootfs,'block_devices':partitions,'machine_id':pathlib.Path('/etc/machine-id').read_text().strip(),'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip(),'hostname':subprocess.check_output(['hostname'],text=True).strip(),'ssh_host_key':subprocess.check_output(['ssh-keygen','-lf','/etc/ssh/ssh_host_ed25519_key.pub'],text=True).strip(),'runtime':json.loads(r.stdout),'assets':{str(p):__import__('hashlib').sha256(p.read_bytes()).hexdigest() for p in pathlib.Path('/opt/nomad-workers').iterdir() if p.is_file()},'timestamp':time.time()}))
"""
 for index in indices:
  name=a.acceptance_prefix+'-'+str(index);start=time.monotonic();r=subprocess.run([*lim,'shell',name,'/usr/bin/python3','-'],input=script,text=True,capture_output=True,timeout=240);assert r.returncode==0,r.stderr[-2000:]
  check=json.loads(r.stdout);probe=subprocess.run([*lim,'shell',name,'/usr/bin/python3','-'],input=(root/'base-native-output-probe.py').read_text(),text=True,capture_output=True,timeout=90);assert probe.returncode==0,'no-model native output probe failed';check['native_output']=json.loads(probe.stdout);check['name']=name;check['check_seconds']=time.monotonic()-start;check['requested_disk_gib']=receipt['disk']['minimum_disk_gib'];check['disk_sha256']=receipt['disk']['sha256'];check['architecture']=a.architecture;check['vm_type']=a.vm_type;assert check['root_filesystem']['total_bytes']<receipt['disk']['virtual_size_bytes'];save(out/('boot-check-'+str(index)+'.json'),check);stage('independent_runtime_pass',name=name,root_filesystem=check['root_filesystem'])
 for index in (1,2):
  saved=out/('boot-check-'+str(index)+'.json')
  if not saved.exists():stage('independent_boot_probe_saved_waiting_second');sys.exit(0)
  proof=json.loads(saved.read_text());assert proof['disk_sha256']==receipt['disk']['sha256'];assert proof.get('architecture', 'amd64')==a.architecture and proof.get('vm_type','qemu')==a.vm_type;checks.append(proof)
 expected=json.loads((root/'seal-receipt.json').read_text())['guest_assets']
 validate_boot_proofs(checks,expected,receipt['disk']['sha256'],platform)
 assert sha(out/platform.filename)==receipt['disk']['sha256']
 save(out/'acceptance.json',{'sealed':True,'independent_boots':2,'unique_identities':True,'base_sha256_unchanged':receipt['disk']['sha256'],'boots':checks})
 stage('two_independent_boots_pass',disk_sha256=receipt['disk']['sha256'])

if a.phase=='catalog':
 assert a.version and __import__('re').fullmatch('[a-z0-9][a-z0-9._-]{0,79}',a.version)
 assert __import__('re').fullmatch('[a-z0-9][a-z0-9._-]{0,79}',a.template_id)
 export=json.loads((out/'export.json').read_text());accept=json.loads((out/'acceptance.json').read_text())
 assert accept['sealed'] and accept['independent_boots']==2 and accept['unique_identities']
 assert sha(out/platform.filename)==export['disk']['sha256']==accept['base_sha256_unchanged']
 runtime=out/'runtime.yaml'
 entry={'template_id':a.template_id,'version':a.version,'guest_os':'linux','architecture':a.architecture,'vm_type':a.vm_type,'disk':dict(export['disk'],url=(out/platform.filename).as_uri()),'runtime_yaml':{'url':runtime.as_uri(),'sha256':sha(runtime),'size_bytes':runtime.stat().st_size},'tools':{'prime':'0.10.1','acpx':'0.21.0','node':'24.22.1','bao':'2.5.4'},'acceptance':{k:accept[k] for k in ('sealed','independent_boots','unique_identities')}}
 model_proof=None
 if a.model_catalog_proof:
  proof_path=pathlib.Path(a.model_catalog_proof);assert not proof_path.is_symlink()and proof_path.resolve().parent==out.resolve(),'Model proof must be inside own artifact directory'
  model_proof=json.loads(proof_path.read_text());entry['tools']['prime_model_catalog']=model_catalog_capability(model_proof,export['disk']['sha256'],json.loads((root/'seal-receipt.json').read_text())['guest_assets'])
 catalog={'schema_version':1,'templates':[entry]};target=out/'catalog.json'
 if target.exists():assert json.loads(target.read_text())==catalog
 else:save(target,catalog)
 save(out/'metadata.json',{'schema_version':1,'template':entry,'prepare':json.loads((root/'prepare-operation.json').read_text()),'seal':json.loads((root/'seal-receipt.json').read_text()),'export':export,'acceptance':accept,'native_model_catalog_acceptance':model_proof})
 stage('local_catalog_ready',catalog_sha256=sha(target),disk_sha256=entry['disk']['sha256'])

if a.phase=='stop':
 if a.boot_index:assert (out/('boot-check-'+str(a.boot_index)+'.json')).exists(),'selected boot probe must pass before stop'
 else:assert (out/'acceptance.json').exists(),'do not stop tests before acceptance evidence'
 for index in indices:
  name=a.acceptance_prefix+'-'+str(index)
  instances=[json.loads(line) for line in run([*lim,'list','--json',name]).splitlines() if line.strip()]
  instance=next(x for x in instances if x['name']==name)
  assert instance['dir']==str(home/name), 'Unexpected acceptance instance path'
  if instance['status']!='Stopped':run([*lim,'stop','--tty=false',name],timeout=240)
  check_stopped_disk(name)
 receipt={'stopped_at':datetime.now(timezone.utc).isoformat(),'names':[a.acceptance_prefix+'-'+str(i)for i in indices],'disks_retained':True}
 save(root/('acceptance-stopped-'+str(a.boot_index)+'.json' if a.boot_index else 'acceptance-stopped.json'),receipt)
 stage('selected_acceptance_vms_stopped_disks_retained',names=receipt['names'])
