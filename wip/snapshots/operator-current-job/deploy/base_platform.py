"""Portable native Lima image primitives. No VM or process lifecycle actions."""
import dataclasses,fcntl,hashlib,os,pathlib,platform,struct,zlib
@dataclasses.dataclass(frozen=True)
class Platform:
 architecture:str='amd64'
 vm_type:str='qemu'
 def __post_init__(self):
  if self.architecture not in ('amd64','arm64')or self.vm_type not in ('qemu','vz'):raise ValueError('Unsupported platform')
  if self.vm_type=='vz'and self.architecture!='arm64':raise ValueError('Only native ARM64 VZ accepted')
 @property
 def lima_arch(self):return {'amd64':'x86_64','arm64':'aarch64'}[self.architecture]
 @property
 def format(self):return 'raw'if self.vm_type=='vz'else'qcow2'
 @property
 def filename(self):return 'disk.'+self.format
 def require_host(self,system=None,machine=None):
  system=system or platform.system();machine=machine or platform.machine()
  if machine not in ({'amd64','x86_64'}if self.architecture=='amd64'else{'arm64','aarch64'}):raise ValueError('Native architecture required')
  if system!=('Darwin'if self.vm_type=='vz'else'Linux'):raise ValueError('Backend requires matching host')
 def runtime_yaml(self):
  return 'minimumLimaVersion: 2.2.1\nvmType: '+self.vm_type+'\narch: '+self.lima_arch+'\nplain: true\npropagateProxyEnv: false\nssh:\n  forwardAgent: false\n  loadDotSSHPubKeys: false\ncontainerd:\n  system: false\n  user: false\n'
def digest(path):
 h=hashlib.sha256()
 with open(path,'rb')as f:
  for b in iter(lambda:f.read(8*1024*1024),b''):h.update(b)
 return h.hexdigest()
def raw_gpt_info(path,expected_size):
 p=pathlib.Path(path);fd=os.open(p,os.O_RDONLY|os.O_NOFOLLOW)
 try:
  st=os.fstat(fd)
  if not __import__('stat').S_ISREG(st.st_mode)or st.st_size!=expected_size or expected_size%512:raise ValueError('Unexpected raw disk size/type')
  def header(lba):
   b=os.pread(fd,512,lba*512)
   if b[:8]!=b'EFI PART':raise ValueError('Raw GPT header required; no qcow2/ASIF/backing chain accepted')
   size=struct.unpack_from('<I',b,12)[0]
   if not 92<=size<=512:raise ValueError('Invalid GPT header size')
   buf=bytearray(b[:size]);crc=struct.unpack_from('<I',buf,16)[0];struct.pack_into('<I',buf,16,0)
   if zlib.crc32(buf)&0xffffffff!=crc:raise ValueError('GPT header checksum mismatch')
   return b
  primary=header(1);backup=header(expected_size//512-1)
  if primary[56:72]!=backup[56:72]or struct.unpack_from('<Q',primary,32)[0]!=expected_size//512-1:raise ValueError('GPT backup/identity mismatch')
  return {'format':'raw','virtual-size':st.st_size,'backing-filename':None}
 finally:os.close(fd)
def export_raw(source,dest,expected_size,stopped_guard):
 """Copy a proven stopped standalone raw GPT disk, preserving the original.
 Caller must verify exact Lima identity/status, PID absence and lsof on each guard.
 Existing partial output fails closed; no resumed/repeated copy on unknown outcome.
 """
 source=pathlib.Path(source);dest=pathlib.Path(dest);partial=dest.with_name(dest.name+'.partial')
 stopped_guard();raw_gpt_info(source,expected_size)
 fd=os.open(source,os.O_RDONLY|os.O_NOFOLLOW)
 try:
  fcntl.flock(fd,fcntl.LOCK_EX|fcntl.LOCK_NB);before=os.fstat(fd);original=digest(source)
  if dest.exists()or dest.is_symlink():raise FileExistsError(dest)
  out=os.open(partial,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  try:
   while True:
    block=os.read(fd,1024*1024)
    if not block:break
    if block==bytes(len(block)):os.lseek(out,len(block),os.SEEK_CUR)
    else:
     view=memoryview(block)
     while view:view=view[os.write(out,view):]
   os.ftruncate(out,expected_size);os.fsync(out)
  finally:os.close(out)
  stopped_guard();after=source.stat()
  if (before.st_dev,before.st_ino,before.st_size,before.st_mtime_ns)!=(after.st_dev,after.st_ino,after.st_size,after.st_mtime_ns)or digest(source)!=original or digest(partial)!=original:raise ValueError('Source changed or export checksum mismatch; preserve partial for inspection')
  raw_gpt_info(partial,expected_size);os.chmod(partial,0o444);os.link(partial,dest,follow_symlinks=False);partial.unlink()
  dirfd=os.open(dest.parent,os.O_RDONLY)
  try:os.fsync(dirfd)
  finally:os.close(dirfd)
  return {'format':'raw','virtual-size':expected_size,'backing-filename':None}
 finally:os.close(fd)
def build_disk_budget_gib(disk_gib,reserve_gib=20):
 if type(disk_gib)is not int or disk_gib<8 or reserve_gib<20:raise ValueError('Explicit disk and host reserve required')
 # Builder + immutable export + two retained acceptance disks + source cache + temp.
 return 6*disk_gib+reserve_gib
def validate_boot_proofs(checks,expected_assets,disk_sha,selected):
 if len(checks)!=2:raise ValueError('Two independent boot proofs required')
 for proof in checks:
  if proof['disk_sha256']!=disk_sha or proof.get('architecture','amd64')!=selected.architecture or proof.get('vm_type','qemu')!=selected.vm_type:raise ValueError('Boot proof platform/digest mismatch')
  if {pathlib.Path(k).name:v for k,v in proof['assets'].items()}!=expected_assets:raise ValueError('Boot proof frozen asset mismatch')
 for field in ('machine_id','boot_id','hostname','ssh_host_key'):
  if not checks[0].get(field)or checks[0][field]==checks[1].get(field):raise ValueError('Missing or duplicated guest identity: '+field)
def model_catalog_capability(proof,disk_sha,guest_assets):
 models=proof.get('native_models_advertised',[])
 if proof.get('disk_sha256')!=disk_sha or proof.get('guest_assets')!=guest_assets:raise ValueError('Native model proof belongs to another image/freeze')
 if len(models)!=10 or len(set(models))!=10 or proof.get('native_model_count')!=10 or proof.get('compatibility_groups')!=2:raise ValueError('Full native model catalog proof required')
 if proof.get('actual_acpx_control')!='pass'or proof.get('guest_model_generator_verified')is not True or proof.get('model_requests')!=0 or proof.get('worker_tasks_started')!=0:raise ValueError('No-prompt native Prime/acpx proof required')
 if proof.get('coordinator_selections')!=['gpt-6.1-sol','deepseek-v4.1-flash-auto']or not proof.get('acpx_session_closed')or not proof.get('prime_daemon_and_direct_transport_stopped'):raise ValueError('Native coordinator control/cleanup proof required')
 return '1'
