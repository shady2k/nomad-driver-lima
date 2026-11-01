# Fixed per-task bootstrap. Not an agent, RPC endpoint, or persistent service.
import os,sys,json,time,subprocess,base64,signal
p=json.load(sys.stdin);key=p['key'];owner=p['owner']
assert len(key)==64 and all(c in '0123456789abcdef' for c in key)
b='/var/tmp/nomad-lima-'+key

def atomic(name,value):
 t=b+'/'+name+'.tmp-'+str(os.getpid())
 with open(t,'w') as f:json.dump(value,f);f.flush();os.fsync(f.fileno())
 os.replace(t,b+'/'+name)
def load(name):
 try:
  with open(b+'/'+name) as f:return json.load(f)
 except FileNotFoundError:return None
def proc(pid):
 try:
  s=open('/proc/'+str(pid)+'/stat').read().split(') ',1)[1].split()
  return s[0],s[19]
 except (OSError,IndexError):return None,None
os.umask(0o077)
if p['action']=='identity':
 print(json.dumps({'machine_id':open('/etc/machine-id').read().strip(),'boot_id':open('/proc/sys/kernel/random/boot_id').read().strip()}));sys.exit(0)
if p['action']=='resource_status':
 # Read-only hardware/filesystem evidence. Unsupported layouts stay unverified.
 r={'cpus':os.cpu_count(),'memory_bytes':0,'disk_bytes':0,'partition_bytes':0,'filesystem_bytes':0,'fs_type':'','fs_uuid':'','direct_partition':False}
 for line in open('/proc/meminfo'):
  if line.startswith('MemTotal:'):r['memory_bytes']=int(line.split()[1])*1024
 try:
  mount=json.loads(subprocess.check_output(['findmnt','--json','--target','/','--output','SOURCE,FSTYPE'],timeout=5))['filesystems'][0]
  source=mount['source'];assert source.startswith('/dev/') and '[' not in source
  tree=json.loads(subprocess.check_output(['lsblk','--inverse','--bytes','--json','--paths','--output','NAME,TYPE,SIZE,FSTYPE,UUID',source],timeout=5))['blockdevices']
  def flat(rows):
   for row in rows:
    yield row
    yield from flat(row.get('children',[]))
  rows=list(flat(tree));parts=[x for x in rows if x['type']=='part'];disks=[x for x in rows if x['type']=='disk']
  assert len(parts)==1 and len(disks)==1 and len(rows)==2 and mount['fstype'] in ('ext4','xfs')
  fs=os.statvfs('/');r.update(disk_bytes=int(disks[0]['size']),partition_bytes=int(parts[0]['size']),filesystem_bytes=fs.f_blocks*fs.f_frsize,fs_type=mount['fstype'],fs_uuid=parts[0]['uuid'] or '',direct_partition=True)
 except Exception:pass
 print(json.dumps({'resources':r,'machine_id':open('/etc/machine-id').read().strip(),'boot_id':open('/proc/sys/kernel/random/boot_id').read().strip()}));sys.exit(0)
if p['action']=='launch':
 os.makedirs(b,exist_ok=True,mode=0o700)
 marker=load('owner.json')
 if marker is None:atomic('owner.json',{'owner':owner,'key':key})
 else:assert marker=={'owner':owner,'key':key},'guest owner mismatch'
 if load('launch.json') is None:
  # Exclusive intent is irreversible: uncertain launch is never replayed.
  fd=os.open(b+'/launch.json',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
  with os.fdopen(fd,'w') as f:json.dump({'created':time.time(),'command':p['command']},f);f.flush();os.fsync(f.fileno())
  worker=r'''
import os,sys,json,time,subprocess,signal
b=sys.argv[1]
def atomic(n,v):
 t=b+'/'+n+'.tmp-'+str(os.getpid())
 with open(t,'w') as f:json.dump(v,f);f.flush();os.fsync(f.fileno())
 os.replace(t,b+'/'+n)
def ticks(pid):return open('/proc/'+str(pid)+'/stat').read().split(') ',1)[1].split()[19]
os.umask(0o077)
atomic('worker.json',{'pid':os.getpid(),'ticks':ticks(os.getpid())})
try:
 spec=json.load(open(b+'/launch.json'))
 with open(b+'/stdout','ab',buffering=0) as out,open(b+'/stderr','ab',buffering=0) as err:
  child=subprocess.Popen(spec['command'],stdin=subprocess.DEVNULL,stdout=out,stderr=err,cwd='/tmp',start_new_session=True,close_fds=True)
  atomic('child.json',{'pid':child.pid,'ticks':ticks(child.pid)})
  code=child.wait()
 result={'code':code if code>=0 else 128-code,'signal':-code if code<0 else 0,'error':''}
except Exception as e:result={'code':127,'signal':0,'error':type(e).__name__}
atomic('result.json',result)
'''
  subprocess.Popen(['/usr/bin/python3','-c',worker,b],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True,close_fds=True)
else:
 assert load('owner.json')=={'owner':owner,'key':key},'guest owner mismatch'
if p['action']=='signal':
 d=load('child.json');assert d is not None,'no child identity'
 state,ticks=proc(d['pid']);assert ticks==d['ticks'] and state!='Z','child not active'
 os.killpg(d['pid'],int(p.get('signal',15)))
result=load('result.json');worker=load('worker.json');launch=load('launch.json')
phase='launching'
if result is not None:phase='exited'
elif worker:
 state,ticks=proc(worker['pid']);phase='running' if ticks==worker['ticks'] and state!='Z' else 'unknown'
elif launch and time.time()-launch['created']>30:phase='unknown'
reply={'phase':phase,'result':result,'machine_id':open('/etc/machine-id').read().strip(),'boot_id':open('/proc/sys/kernel/random/boot_id').read().strip()}
for stream in ['stdout','stderr']:
 off=int(p.get(stream+'_offset',0));assert off>=0
 try:
  with open(b+'/'+stream,'rb') as f:
   f.seek(off);chunk=f.read(65536);eof=f.tell()>=os.fstat(f.fileno()).st_size
 except FileNotFoundError:chunk=b'';eof=True
 reply[stream]=base64.b64encode(chunk).decode();reply[stream+'_offset']=off+len(chunk);reply[stream+'_eof']=eof
print(json.dumps(reply))
