"""Read-only validation of an explicitly approved stopped-source receipt."""
import hashlib,json,os,pathlib,re,subprocess
import sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save

def verify(packet,context,state,digest,run=subprocess.run):
 recovery=packet['plan'].get('source_recovery')
 if not recovery:raise RuntimeError('EXPLICIT_STOPPED_SOURCE_RECOVERY_REQUIRED')
 p=packet['plan'];receipt=save.recovery_contract(recovery['receipt'],p['source'])
 path=pathlib.Path(context['source_recovery_receipt_file'])
 if path.is_symlink()or path.stat().st_uid!=os.getuid()or path.stat().st_mode&0o077:raise RuntimeError('PRIVATE_OPERATOR_RECOVERY_RECEIPT_REQUIRED')
 actual=json.loads(path.read_text());identity=save.sha(actual)
 if identity!=recovery['receipt_sha256']or identity!=context.get('source_recovery_receipt_sha256')or actual!=receipt or actual['approval_reference']!=context.get('source_stop_approval_reference'):raise RuntimeError('PINNED_OPERATOR_RECOVERY_APPROVAL_CHANGED')
 if digest!=receipt['ledger_sha256']or state.get('Instance')!=receipt['instance']or state.get('Phase')!='exited'or state.get('VaultFinalizing')is not True:raise RuntimeError('STOPPED_RECOVERY_LEDGER_CHANGED')
 root=pathlib.Path(context['lima_home'])/receipt['instance']
 if root.is_symlink()or not root.is_dir():raise RuntimeError('EXACT_STOPPED_RECOVERY_INSTANCE_REQUIRED')
 import pwd
 user=pwd.getpwnam(context['runtime_user']);args=['/usr/sbin/runuser','-u',user.pw_name,'--','/usr/bin/env','-i','HOME='+user.pw_dir,'PATH=/usr/local/bin:/usr/bin:/bin','LIMA_HOME='+context['lima_home'],context['limactl'],'list','--json',receipt['instance']]
 r=run(args,text=True,capture_output=True,timeout=20);rows=[json.loads(v)for v in r.stdout.splitlines()if v]if r.returncode==0 else[]
 if len(rows)!=1 or rows[0].get('name')!=receipt['instance']or rows[0].get('dir')!=str(root)or rows[0].get('status')!='Stopped':raise RuntimeError('RECOVERY_SOURCE_NOT_PHYSICALLY_STOPPED')
 for file in root.glob('*.pid'):
  if file.is_symlink():raise RuntimeError('UNSAFE_SOURCE_PID_METADATA')
  text=file.read_text().strip()
  if not text.isdigit():raise RuntimeError('UNCERTAIN_SOURCE_PID_METADATA')
  try:os.kill(int(text),0)
  except ProcessLookupError:continue
  raise RuntimeError('SOURCE_PROCESS_STILL_EXISTS')
 disk=root/'disk'
 if disk.is_symlink()or not disk.is_file()or disk.stat().st_size!=receipt['disk_size_bytes']:raise RuntimeError('RECOVERY_SOURCE_DISK_SIZE_CHANGED')
 h=hashlib.sha256()
 with disk.open('rb')as f:
  for data in iter(lambda:f.read(1048576),b''):h.update(data)
 if h.hexdigest()!=receipt['disk_sha256']:raise RuntimeError('RECOVERY_SOURCE_DISK_HASH_CHANGED')
 return receipt
