#!/usr/bin/env python3
"""Fixed candidate-only preboot cleanup with upstream libguestfs appliance.
No host mounts, guest command execution, network, token reads or source writes.
"""
import hashlib,json,os,pathlib,pwd,re,subprocess,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save as save

def clean(packet,receipts,context,run=subprocess.run):
 p=save.validate_operation(packet);clone=save.verify_stage('clone_candidate',receipts['clone_candidate'],packet)
 if not p.get('source_recovery')or context.get('vm_type')!='qemu':raise RuntimeError('STOPPED_QEMU_RECOVERY_REQUIRED')
 tool=context.get('guestfish')
 if not tool or not pathlib.Path(tool).is_file()or not os.access(tool,os.X_OK):raise RuntimeError('OFFLINE_SANITATION_TOOL_REQUIRED_BEFORE_CLONE')
 candidate=clone.get('candidate_instance');expected='agent-base-build-save-'+p['operation_id'].replace('-','')
 if candidate!=expected:raise RuntimeError('EXACT_CANDIDATE_REQUIRED')
 home=pathlib.Path(context['lima_home']);source=home/p['source_recovery']['receipt']['instance']/'disk';disk=home/candidate/'disk'
 if disk.is_symlink()or source.is_symlink()or not disk.is_file()or not source.is_file()or os.path.samestat(source.stat(),disk.stat()):raise RuntimeError('INDEPENDENT_CANDIDATE_DISK_REQUIRED')
 def sha(path):
  h=hashlib.sha256()
  with path.open('rb')as f:
   for part in iter(lambda:f.read(1048576),b''):h.update(part)
  return h.hexdigest()
 before=sha(source)
 if before!=p['source_recovery']['receipt']['disk_sha256']:raise RuntimeError('RECOVERY_SOURCE_CHANGED_BEFORE_CLEANUP')
 user=pwd.getpwnam(context['runtime_user']);base=['/usr/sbin/runuser','-u',user.pw_name,'--','/usr/bin/env','-i','HOME='+user.pw_dir,'PATH=/usr/local/bin:/usr/bin:/bin','LIMA_HOME='+str(home)]
 def stopped(name):
  result=run([*base,context['limactl'],'list','--json',name],text=True,capture_output=True,timeout=20);rows=[json.loads(x)for x in result.stdout.splitlines()if x]if result.returncode==0 else[]
  if len(rows)!=1 or rows[0].get('name')!=name or rows[0].get('status')!='Stopped'or rows[0].get('dir')!=str(home/name):raise RuntimeError('CANDIDATE_AND_SOURCE_MUST_STAY_STOPPED')
 stopped(candidate);stopped(p['source_recovery']['receipt']['instance'])
 paths=['/var/tmp/nomad-workers','/opt/worker/state','/opt/worker/secrets','/root/.prime','/root/.acpx','/root/.config/prime','/root/.bao-token','/root/.vault-token']+save.composition_contract(p['data_composition'])['remove']
 if any(not re.fullmatch(r'/[A-Za-z0-9_./-]+',v)or '..'in v.split('/')for v in paths):raise RuntimeError('FIXED_OFFLINE_DATA_PATHS_REQUIRED')
 commands=[]
 for path in dict.fromkeys(paths):commands.extend(['rm-rf',path,':'])
 for pattern in ('/home/*/.prime','/home/*/.acpx','/home/*/.config/prime','/home/*/.bao-token','/home/*/.vault-token'):
  commands.extend(['glob','rm-rf',pattern,':'])
 commands.extend(['sync',':','umount-all'])
 # --network is deliberately absent: libguestfs networking defaults false.
 # direct backend uses the official appliance, not a libvirt service or host FS.
 result=run([*base,'LIBGUESTFS_BACKEND=direct',tool,'--rw','--format=qcow2','-a',str(disk),'-i',*commands],text=True,capture_output=True,timeout=1200)
 if result.returncode:raise RuntimeError('OFFLINE_CLEANUP_FAILED_CANDIDATE_RETAINED')
 stopped(candidate);stopped(p['source_recovery']['receipt']['instance'])
 if sha(source)!=before:raise RuntimeError('SOURCE_CHANGED_DURING_CANDIDATE_CLEANUP')
 return save.verify_stage('offline_cleanup',{'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'offline_cleanup_before_boot':True,'candidate_stopped':True,'source_disk_unchanged':True,'runtime_state_removed':True,'revocation':'unconfirmed','source_ledger_rewritten':False},packet)
if __name__=='__main__':
 try:
  context=exchange.protected(sys.argv[1]);payload=json.load(sys.stdin);print(json.dumps(clean(payload['packet'],payload['receipts'],context)))
 except Exception as e:
  print(json.dumps({'error':str(e)if re.fullmatch('[A-Z_]{1,100}',str(e))else type(e).__name__}),file=sys.stderr);sys.exit(1)
