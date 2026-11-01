"""Finite input-file transfer through existing native exec; no agent/SSH/Git."""
import os,stat,pathlib,json,hashlib,tempfile,fcntl,contextlib,urllib.request,urllib.parse,tarfile,shutil,uuid,ctypes,sys,gzip
PART=8*1024*1024
MAX_FILE=256*1024*1024

def fail(code):raise RuntimeError(code)
def digest_fd(fd):
 os.lseek(fd,0,0);h=hashlib.sha256();size=0
 while True:
  chunk=os.read(fd,1024*1024)
  if not chunk:break
  h.update(chunk);size+=len(chunk)
 return size,h.hexdigest()
def atomic(path,value):
 fd,tmp=tempfile.mkstemp(prefix='.upload-',dir=path.parent)
 try:
  with os.fdopen(fd,'w')as f:os.fchmod(f.fileno(),0o600);json.dump(value,f,sort_keys=True);f.flush();os.fsync(f.fileno())
  os.replace(tmp,path);fd=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
  try:os.fsync(fd)
  finally:os.close(fd)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def private(path,directory=False):
 st=path.lstat()
 if stat.S_ISLNK(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077 or not(stat.S_ISDIR(st.st_mode)if directory else stat.S_ISREG(st.st_mode)):fail('UPLOAD_PRIVATE_STATE_REQUIRED')
 return st
def read(path):
 if private(path).st_size>2097152:fail('UPLOAD_STATE_BOUND')
 return json.loads(path.read_text())
def relative(value):
 if not isinstance(value,str)or not value or len(value.encode())>1024 or '\\'in value or '\0'in value or any(ord(x)<32 for x in value):fail('UPLOAD_DESTINATION_INVALID')
 path=pathlib.PurePosixPath(value)
 if path.is_absolute()or any(x in ('','.','..')for x in value.split('/'))or len(path.parts)>64:fail('UPLOAD_DESTINATION_INVALID')
 return path.parts
@contextlib.contextmanager
def parent_fd(root,value,create=True):
 parts=relative(value);fd=os.dup(root)
 try:
  for name in parts[:-1]:
   if create:
    try:os.mkdir(name,0o700,dir_fd=fd)
    except FileExistsError:pass
   child=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd);os.close(fd);fd=child
  yield fd,parts[-1]
 finally:os.close(fd)
@contextlib.contextmanager
def parent(workspace,value,create=True):
 root=os.open(workspace,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
 try:
  with parent_fd(root,value,create)as result:yield result
 finally:os.close(root)
def identity():return {'machine_id':pathlib.Path('/etc/machine-id').read_text().strip(),'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args,**kw):fail('UPLOAD_REDIRECT_REFUSED')
def fetch(url,origin,start,count,total):
 parsed=urllib.parse.urlsplit(url);base=urllib.parse.urlsplit(origin)
 if parsed.scheme!='https'or parsed.netloc!=base.netloc or parsed.username or parsed.password or parsed.fragment or base.scheme!='https':fail('UPLOAD_UNAPPROVED_OBJECT_ORIGIN')
 request=urllib.request.Request(url,headers={'Range':f'bytes={start}-{start+count-1}'})
 with urllib.request.build_opener(NoRedirect).open(request,timeout=30)as response:
  if response.status!=206 or response.headers.get('Content-Range')!=f'bytes {start}-{start+count-1}/{total}':fail('UPLOAD_RANGE_RESPONSE_REQUIRED')
  body=response.read(count+1)
 if len(body)!=count:fail('UPLOAD_RANGE_SIZE_CHANGED')
 return body

def no_replace(folder,source,target):
 libc=ctypes.CDLL(None,use_errno=True)
 if sys.platform=='linux':function=getattr(libc,'renameat2',None);flags=1
 elif sys.platform=='darwin':function=getattr(libc,'renameatx_np',None);flags=4
 else:function=None;flags=0
 if function is None:fail('UPLOAD_ATOMIC_DIRECTORY_PUBLISH_UNSUPPORTED')
 if function(folder,os.fsencode(source),folder,os.fsencode(target),flags)!=0:raise OSError(ctypes.get_errno(),'UPLOAD_DIRECTORY_PUBLISH_REFUSED')
def archive_preflight(archive):
 """Bound extended headers before tarfile can allocate/decode their payload."""
 archive.seek(0);magic=archive.read(6);archive.seek(0)
 if magic.startswith(b'\x1f\x8b'):stream=gzip.GzipFile(fileobj=archive,mode='rb')
 elif magic.startswith((b'BZh',b'\xfd7zXZ',b'\x28\xb5\x2f\xfd')):fail('UPLOAD_ARCHIVE_COMPRESSION_UNSUPPORTED')
 else:stream=archive
 total=metadata=0
 try:
  for number in range(8192):
   block=stream.read(512)
   if not block or block==b'\0'*512:return
   if len(block)!=512:fail('UPLOAD_ARCHIVE_HEADER_INVALID')
   try:item=tarfile.TarInfo.frombuf(block,'utf-8','strict')
   except tarfile.TarError:fail('UPLOAD_ARCHIVE_HEADER_INVALID')
   if item.type==tarfile.GNUTYPE_SPARSE:fail('UPLOAD_ARCHIVE_SPARSE_FORBIDDEN')
   extended=item.type in (tarfile.XHDTYPE,tarfile.XGLTYPE,tarfile.GNUTYPE_LONGNAME,tarfile.GNUTYPE_LONGLINK)
   if item.size<0 or item.size>MAX_FILE:fail('UPLOAD_ARCHIVE_SIZE_BOUND')
   if extended:
    metadata+=item.size
    if item.size>65536 or metadata>1048576:fail('UPLOAD_ARCHIVE_METADATA_BOUND')
   total+=512+((item.size+511)//512)*512
   if total>1024**3+8*1024**2:fail('UPLOAD_ARCHIVE_SIZE_BOUND')
   remaining=((item.size+511)//512)*512;content=b''
   while remaining:
    chunk=stream.read(min(1024*1024,remaining))
    if not chunk:fail('UPLOAD_ARCHIVE_TRUNCATED')
    if extended:content+=chunk
    remaining-=len(chunk)
   if item.type in (tarfile.XHDTYPE,tarfile.XGLTYPE)and b'GNU.sparse'in content:fail('UPLOAD_ARCHIVE_SPARSE_FORBIDDEN')
  fail('UPLOAD_ARCHIVE_FILE_COUNT_BOUND')
 finally:
  if stream is not archive:stream.close()
  archive.seek(0)

def extract(archive,workspace,destination,state,record):
 relative(destination);archive_preflight(archive)
 with parent(workspace,destination)as(fd,name):
  if record.get('extract_verified')or record.get('extract_publish_intent'):
   try:published=os.open(name,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
   except FileNotFoundError:published=None
   if published is not None:
    try:
     st=os.fstat(published)
     if record.get('extract_identity')!={'dev':st.st_dev,'ino':st.st_ino}:fail('UPLOAD_EXTRACT_DESTINATION_EXISTS')
     for item in record['extracted_files']:
      with parent_fd(published,item['path'])as(pfd,base):
       file=os.open(base,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=pfd)
       try:
        if not stat.S_ISREG(os.fstat(file).st_mode)or digest_fd(file)!=(item['size'],item['sha256']):fail('UPLOAD_EXTRACT_CHANGED_NO_OVERWRITE')
       finally:os.close(file)
     record['extract_verified']=True;atomic(state,record);return
    finally:os.close(published)
  try:os.stat(name,dir_fd=fd,follow_symlinks=False)
  except FileNotFoundError:pass
  else:fail('UPLOAD_EXTRACT_DESTINATION_EXISTS')
  staging=record.get('extract_staging')
  if staging is None:
   staging='.nomad-extract-'+str(uuid.uuid4());os.mkdir(staging,0o700,dir_fd=fd);record['extract_staging']=staging
  root=os.open(staging,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  try:
   st=os.fstat(root);inode={'dev':st.st_dev,'ino':st.st_ino}
   if st.st_uid!=os.getuid()or st.st_mode&0o077 or(record.get('extract_identity')and record['extract_identity']!=inode):fail('UPLOAD_EXTRACT_STAGING_CHANGED')
   record['extract_identity']=inode;atomic(state,record);files=[];total=0;seen=set();archive.seek(0)
   with tarfile.open(fileobj=archive,mode='r:*')as tar:
    for number,item in enumerate(tar):
     if number>=4096:fail('UPLOAD_ARCHIVE_FILE_COUNT_BOUND')
     member=item.name.rstrip('/')
     while member.startswith('./'):member=member[2:]
     if member in ('','.')and item.isdir():continue
     names=relative(member);rel='/'.join(names)
     if len(rel.encode())>256:fail('UPLOAD_ARCHIVE_PATH_BOUND')
     if rel in seen:fail('UPLOAD_ARCHIVE_DUPLICATE_PATH')
     seen.add(rel)
     if not(item.isfile()or item.isdir())or item.issym()or item.islnk():fail('UPLOAD_ARCHIVE_TYPE_FORBIDDEN')
     total+=item.size
     if item.size<0 or item.size>MAX_FILE or total>1024**3:fail('UPLOAD_ARCHIVE_SIZE_BOUND')
     with parent_fd(root,rel)as(pfd,base):
      if item.isdir():
       try:os.mkdir(base,0o700,dir_fd=pfd)
       except FileExistsError:
        check=os.open(base,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=pfd);os.close(check)
       continue
      file=os.open(base,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600,dir_fd=pfd)
      try:
       st=os.fstat(file)
       if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077 or st.st_nlink!=1:fail('UPLOAD_ARCHIVE_STAGING_PATH_CHANGED')
       # Only this upload's private unpublished staging file is rewritten.
       os.ftruncate(file,0);h=hashlib.sha256();size=0
       with tar.extractfile(item)as source:
        while True:
         chunk=source.read(1024*1024)
         if not chunk:break
         h.update(chunk);size+=len(chunk);view=memoryview(chunk)
         while view:view=view[os.write(file,view):]
       os.fsync(file)
       if size!=item.size:fail('UPLOAD_ARCHIVE_TRUNCATED')
       files.append({'path':rel,'size':size,'sha256':h.hexdigest()})
      finally:os.close(file)
   record['extracted_files']=files;record['extract_publish_intent']=True;atomic(state,record);no_replace(fd,staging,name);os.fsync(fd);record['extract_verified']=True;atomic(state,record)
  finally:os.close(root)

def handle(p,read_identity=identity,download=fetch):
 c=p['config'];session=p['session_id'];upload=p['upload_id']
 if str(uuid.UUID(session))!=session or str(uuid.UUID(upload))!=upload or type(p['size'])is not int or not 0<=p['size']<=MAX_FILE:fail('UPLOAD_ID_OR_SIZE_INVALID')
 relative(p['destination'])
 if p.get('action')not in ('status','part','finish','inspect','abort')or not __import__('re').fullmatch('[a-f0-9]{64}',p.get('sha256','')):fail('UPLOAD_ACTION_OR_HASH_INVALID')
 machine=read_identity()
 if p.get('machine_identity')and p['machine_identity']!=machine:fail('UPLOAD_MACHINE_BOOT_CHANGED')
 base=pathlib.Path(c['state_dir']);private(base,True);folder=base/session
 if p['action']=='inspect':
  state=folder/'uploads'/(upload+'.json')
  if not state.exists():return {'phase':'absent','offset':0,'size':p['size'],'sha256':p['sha256'],'machine_identity':machine,'extract_verified':False,'read_only':True}
  private(folder,True);private(state.parent,True);record=read(state)
  bound={k:p[k]for k in ['binding','session_id','upload_id','size','sha256','destination','extract_to']}
  if record['binding']!=bound or record['machine_identity']!=machine:fail('UPLOAD_BINDING_CHANGED')
  if record['phase']=='completed':
   with parent(c['workspace'],p['destination'],False)as(fd,name):
    data=os.open(name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
    try:
     if digest_fd(data)!=(p['size'],p['sha256']):fail('UPLOAD_PUBLISHED_HASH_CHANGED')
    finally:os.close(data)
  return {'phase':'abort_pending'if record.get('abort_intent')and record['phase']!='aborted'else record['phase'],'offset':record['offset'],'size':p['size'],'sha256':p['sha256'],'machine_identity':machine,'extract_verified':bool(record.get('extract_verified')),'read_only':True}

 folder.mkdir(mode=0o700,exist_ok=True);private(folder,True)
 guest=folder/'session.json'
 if guest.exists():
  owned=read(guest)
  if owned.get('config')!=c or owned.get('phase')!='ready':fail('UPLOAD_GUEST_NOT_READY')
 root=folder/'uploads';root.mkdir(mode=0o700,exist_ok=True);private(root,True);state=root/(upload+'.json');lock=os.open(root/(upload+'.lock'),os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
 with os.fdopen(lock,'a')as f:
  fcntl.flock(f,fcntl.LOCK_EX)
  binding={k:p[k]for k in ['binding','session_id','upload_id','size','sha256','destination','extract_to']}
  if state.exists():record=read(state)
  else:record={'binding':binding,'machine_identity':machine,'offset':0,'phase':'receiving'};atomic(state,record)
  if record['binding']!=binding or record['machine_identity']!=machine:fail('UPLOAD_BINDING_CHANGED')
  if p['action']=='abort'or record.get('abort_intent'):
   if record.get('extract_staging')and not record.get('extract_verified'):fail('UPLOAD_EXTRACTION_STAGING_REQUIRES_RECONCILIATION')
   if record['phase']!='aborted':
    record['abort_intent']=True;atomic(state,record)
    if record.get('partial_name'):
     if not __import__('re').fullmatch(r'\.nomad-upload-[a-f0-9-]{36}\.partial',record['partial_name']):fail('UPLOAD_PARTIAL_PATH_CHANGED')
     with parent(c['workspace'],p['destination'],False)as(fd,name):
      st=os.fstat(fd)
      if record.get('partial_parent')!={'dev':st.st_dev,'ino':st.st_ino}:fail('UPLOAD_DESTINATION_PARENT_CHANGED')
      try:partial=os.stat(record['partial_name'],dir_fd=fd,follow_symlinks=False)
      except FileNotFoundError:partial=None
      if partial is not None:
       inode={'dev':partial.st_dev,'ino':partial.st_ino}
       if not stat.S_ISREG(partial.st_mode)or partial.st_uid!=os.getuid()or partial.st_mode&0o077 or partial.st_nlink!=1 or(record.get('partial_identity')and record['partial_identity']!=inode)or(not record.get('partial_identity')and partial.st_size!=0):fail('UPLOAD_PARTIAL_PATH_CHANGED')
       os.unlink(record['partial_name'],dir_fd=fd);os.fsync(fd)
    record['phase']='aborted';atomic(state,record)
  if record['phase']=='aborted':
   return {'phase':'aborted','offset':record['offset'],'size':p['size'],'sha256':p['sha256'],'machine_identity':machine,'extract_verified':bool(record.get('extract_verified')),'published_outputs_retained':True}
  with parent(c['workspace'],p['destination'])as(fd,name):
   parent_identity={'dev':os.fstat(fd).st_dev,'ino':os.fstat(fd).st_ino}
   if record.get('partial_parent')and record['partial_parent']!=parent_identity:fail('UPLOAD_DESTINATION_PARENT_CHANGED')
   if not record.get('partial_name'):
    record.update(partial_parent=parent_identity,partial_name='.nomad-upload-'+str(uuid.uuid4())+'.partial');atomic(state,record)
   partial=record['partial_name']
   try:st=os.stat(name,dir_fd=fd,follow_symlinks=False)
   except FileNotFoundError:st=None
   target=pathlib.Path(c['workspace'])/p['destination']
   if st is not None:
    if not stat.S_ISREG(st.st_mode):fail('UPLOAD_DESTINATION_EXISTS_NO_OVERWRITE')
    check=os.open(name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
    try:
     if digest_fd(check)!=(p['size'],p['sha256']):fail('UPLOAD_DESTINATION_EXISTS_NO_OVERWRITE')
    finally:os.close(check)
    record.update(phase='completed',offset=p['size']);atomic(state,record)
   else:
    flags=os.O_RDWR|os.O_NOFOLLOW|(0 if record.get('partial_identity')else os.O_CREAT|os.O_EXCL)
    try:data=os.open(partial,flags,0o600,dir_fd=fd)
    except FileExistsError:
     if record.get('partial_identity'):raise
     # Durable random path/parent intent predates creation. No part can be
     # written until inode commit, so only an empty private inode can recover.
     data=os.open(partial,os.O_RDWR|os.O_NOFOLLOW,dir_fd=fd)
     if os.fstat(data).st_size!=0:os.close(data);fail('UPLOAD_PARTIAL_CREATION_UNCONFIRMED')
    try:
     st=os.fstat(data)
     if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077 or st.st_nlink!=1:fail('UPLOAD_PARTIAL_PATH_CHANGED')
     inode={'dev':st.st_dev,'ino':st.st_ino}
     if record.get('partial_identity')and record['partial_identity']!=inode:fail('UPLOAD_PARTIAL_PATH_CHANGED')
     record['partial_identity']=inode;atomic(state,record)
     if st.st_size!=record['offset']:
      inflight=record.get('inflight')
      if not inflight or not record['offset']<=st.st_size<=record['offset']+inflight['size']:fail('UPLOAD_PARTIAL_SIZE_CHANGED')
      if st.st_size==record['offset']+inflight['size']:
       os.lseek(data,record['offset'],0);body=os.read(data,inflight['size'])
       if hashlib.sha256(body).hexdigest()!=inflight['sha256']:fail('UPLOAD_PARTIAL_HASH_CHANGED')
       record['offset']+=inflight['size'];record.pop('inflight');atomic(state,record)
      else:os.ftruncate(data,record['offset']);os.fsync(data)
     if p['action']=='part'and record['offset']<p['size']:
      start=p['offset'];count=p['part_size']
      if start!=record['offset']or type(count)is not int or count!=min(PART,p['size']-start):fail('UPLOAD_PART_OFFSET_CHANGED')
      record['inflight']={'size':count,'sha256':p['part_sha256']};atomic(state,record)
      body=download(p['url'],p['origin'],start,count,p['size'])
      if len(body)!=count or hashlib.sha256(body).hexdigest()!=p['part_sha256']:fail('UPLOAD_PART_HASH_CHANGED')
      os.lseek(data,start,0);view=memoryview(body)
      while view:view=view[os.write(data,view):]
      os.fsync(data);record['offset']+=count;record.pop('inflight');atomic(state,record)
     if p['action']=='finish':
      if record['offset']!=p['size']:fail('UPLOAD_INCOMPLETE')
      if digest_fd(data)!=(p['size'],p['sha256']):fail('UPLOAD_FINAL_HASH_CHANGED')
      os.link(partial,name,src_dir_fd=fd,dst_dir_fd=fd,follow_symlinks=False);os.unlink(partial,dir_fd=fd);os.fsync(fd);record['phase']='completed';atomic(state,record)
    finally:os.close(data)
   if p['action']=='finish'and p.get('extract_to'):
    if record['phase']!='completed':fail('UPLOAD_NOT_FINALIZED')
    archive=os.open(name,os.O_RDONLY|os.O_NOFOLLOW,dir_fd=fd)
    with os.fdopen(archive,'rb')as source:extract(source,c['workspace'],p['extract_to'],state,record)
  return {'phase':record['phase'],'offset':record['offset'],'size':p['size'],'sha256':p['sha256'],'machine_identity':machine,'extract_verified':bool(record.get('extract_verified'))}
