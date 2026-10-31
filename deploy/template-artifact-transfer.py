"""Bounded receiver for accepted files via existing fixed operator SSH command."""
import hashlib,json,os,pathlib,selectors,subprocess,tempfile,time
FILES=('catalog.json','acceptance.json','export.json','metadata.json','runtime.yaml','model-catalog-proof.json')
def receive(packet,receipts,context,name,destination,popen=subprocess.Popen):
 command=context.get('artifact_fetch_command')
 if not isinstance(command,list) or not command or not command[0].startswith('/'):raise RuntimeError('FIXED_ARTIFACT_FETCH_BINDING_REQUIRED')
 p=popen(command,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
 p.stdin.write(json.dumps({'action':'file','file':name,'packet':packet,'receipts':receipts}).encode());p.stdin.close()
 selector=selectors.DefaultSelector();selector.register(p.stdout,selectors.EVENT_READ);header=None;pending=b'';size=0;h=hashlib.sha256();fd,tmp=tempfile.mkstemp(prefix='.accepted-',dir=destination.parent);deadline=time.monotonic()+context.get('transfer_timeout_seconds',1800)
 try:
  with os.fdopen(fd,'wb')as f:
   os.fchmod(f.fileno(),0o600)
   while True:
    if time.monotonic()>=deadline:raise RuntimeError('ARTIFACT_TRANSFER_TIMEOUT_DATA_RETAINED')
    if not selector.select(timeout=.2):continue
    data=os.read(p.stdout.fileno(),1048576)
    if not data:break
    if header is None:
     pending+=data
     if b'\n'not in pending:
      if len(pending)>8192:raise RuntimeError('ARTIFACT_TRANSFER_HEADER_BOUND')
      continue
     line,data=pending.split(b'\n',1)
     if len(line)>8192:raise RuntimeError('ARTIFACT_TRANSFER_HEADER_BOUND')
     header=json.loads(line);pending=b''
     if header.get('operation_id')!=packet['plan']['operation_id']or header.get('plan_sha256')!=packet['plan_sha256']or header.get('file')!=name or type(header.get('size_bytes'))is not int or not 0<=header['size_bytes']<=5*1024**3:raise RuntimeError('ARTIFACT_TRANSFER_BINDING_CHANGED')
    if size==0 and os.statvfs(destination.parent).f_bavail*os.statvfs(destination.parent).f_frsize<header['size_bytes']+67108864:raise RuntimeError('PUBLISHER_PHYSICAL_SPACE_REQUIRED')
    size+=len(data)
    if size>header['size_bytes']:raise RuntimeError('ARTIFACT_TRANSFER_SIZE_EXCEEDED')
    f.write(data);h.update(data)
   if p.wait(timeout=5)or header is None or size!=header['size_bytes']or h.hexdigest()!=header.get('sha256'):raise RuntimeError('ARTIFACT_TRANSFER_INCOMPLETE_OR_CORRUPT')
   f.flush();os.fsync(f.fileno())
  if destination.is_symlink():raise RuntimeError('ARTIFACT_DESTINATION_SYMLINK')
  if destination.exists():
   digest=hashlib.sha256()
   with destination.open('rb')as old_file:
    for chunk in iter(lambda:old_file.read(1048576),b''):digest.update(chunk)
   old=digest.hexdigest()
   if old!=header['sha256']:raise RuntimeError('IMMUTABLE_PUBLISHER_COPY_CHANGED')
  else:os.replace(tmp,destination)
  return header
 finally:
  selector.close();p.stdout.close()
  if p.poll()is None:p.terminate();p.wait(timeout=5)
  if os.path.exists(tmp):os.unlink(tmp)
def transfer(packet,receipts,context):
 root=pathlib.Path(context['artifact_root'])/packet['plan']['operation_id'];root.mkdir(parents=True,exist_ok=True,mode=0o700)
 if root.is_symlink() or root.stat().st_mode&0o077:raise RuntimeError('PRIVATE_PUBLISHER_ROOT_REQUIRED')
 artifact=root/'artifact'
 if artifact.is_symlink():raise RuntimeError('PUBLISHER_ARTIFACT_SYMLINK')
 artifact.mkdir(mode=0o700,exist_ok=True)
 for name in FILES:receive(packet,receipts,context,name,artifact/name)
 entry=json.loads((artifact/'catalog.json').read_text())['templates'][0];name='disk.raw'if entry['disk']['format']=='raw'else'disk.qcow2';proof=receive(packet,receipts,context,name,artifact/name)
 if proof['sha256']!=entry['disk']['sha256']or proof['size_bytes']!=entry['disk']['size_bytes']:raise RuntimeError('TRANSFERRED_DISK_DIFFERS_FROM_ACCEPTED_CATALOG')
 return artifact
