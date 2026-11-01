"""Bounded multipart checkpoints; no URLs or signing credentials on disk."""
import contextlib,fcntl,hashlib,json,os,stat,tempfile,uuid,xml.etree.ElementTree as ET
from pathlib import Path
import exchange
PART=8*1024*1024

def atomic(path,doc):
 path=Path(path);path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
 if path.parent.is_symlink() or path.parent.stat().st_uid!=os.getuid() or path.parent.stat().st_mode&0o077:raise exchange.ExchangeError('exchange_checkpoint_directory')
 fd,name=tempfile.mkstemp(prefix='.multipart-',dir=path.parent)
 try:
  with os.fdopen(fd,'wb') as f:f.write(exchange.canonical(doc));f.flush();os.fsync(f.fileno())
  os.replace(name,path);fd=os.open(path.parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW);os.fsync(fd);os.close(fd)
 finally:
  if os.path.exists(name):os.unlink(name)
def nodes(raw,name):return [n for n in ET.fromstring(raw).iter() if n.tag.split('}')[-1]==name]
def text(node,name):return next((n.text for n in node.iter() if n.tag.split('}')[-1]==name),None)
def begin(store,key):
 with store.open('POST',key,b'',0,query={'uploads':''}) as f:raw=f.read(65537)
 if len(raw)>65536:raise exchange.ExchangeError('exchange_multipart_response_limit')
 upload=text(ET.fromstring(raw),'UploadId')
 if not upload or len(upload)>4096:raise exchange.ExchangeError('exchange_multipart_upload_id')
 return upload
def list_parts(store,key,upload):
 with store.open('GET',key,query={'uploadId':upload}) as f:raw=f.read(1048577)
 if len(raw)>1048576:raise exchange.ExchangeError('exchange_multipart_response_limit')
 tree=ET.fromstring(raw)
 if text(tree,'IsTruncated')=='true':raise exchange.ExchangeError('exchange_multipart_list_truncated')
 out={}
 for n in nodes(raw,'Part'):
  number=int(text(n,'PartNumber'));etag=text(n,'ETag');size=int(text(n,'Size'))
  if not 1<=number<=32 or not etag or len(etag)>256:raise exchange.ExchangeError('exchange_multipart_part_metadata')
  out[number]={'etag':etag,'size':size}
 return out
def complete(store,key,upload,parts):
 tree=ET.Element('CompleteMultipartUpload')
 for number,p in sorted(parts.items()):
  n=ET.SubElement(tree,'Part');ET.SubElement(n,'PartNumber').text=str(number);ET.SubElement(n,'ETag').text=p['etag']
 raw=ET.tostring(tree)
 with store.open('POST',key,raw,len(raw),query={'uploadId':upload}) as f:result=f.read(65537)
 if len(result)>65536 or nodes(result,'Error'):raise exchange.ExchangeError('exchange_multipart_complete_failed')

class Manager:
 def __init__(self,store,root):self.store=store;self.root=Path(root)
 @contextlib.contextmanager
 def locked(self,key,size,sha):
  identity=hashlib.sha256(exchange.canonical({'key':key,'size':size,'sha256':sha})).hexdigest();p=self.root/(identity+'.json');self.root.mkdir(mode=0o700,parents=True,exist_ok=True)
  if self.root.is_symlink() or self.root.stat().st_uid!=os.getuid() or self.root.stat().st_mode&0o077:raise exchange.ExchangeError('exchange_checkpoint_directory')
  fd=os.open(self.root/(identity+'.lock'),os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
  try:
   fcntl.flock(fd,fcntl.LOCK_EX)
   if p.exists():d=exchange.protected(p)
   else:d={'version':1,'key':key,'size':size,'sha256':sha,'phase':'new','parts':{}}
   if (d['key'],d['size'],d['sha256'])!=(key,size,sha):raise exchange.ExchangeError('exchange_checkpoint_binding')
   yield p,d
  finally:fcntl.flock(fd,fcntl.LOCK_UN);os.close(fd)
 def plan(self,key,size,sha):
  with self.locked(key,size,sha) as (p,d):
   if d['phase']=='complete':self.store.verify(key,size,sha);return {'complete':True}
   if d['phase']=='initiating':raise exchange.ExchangeError('exchange_multipart_init_outcome_unknown; preserve checkpoint, no blind retry')
   if d['phase']=='new':
    d['phase']='initiating';atomic(p,d);d['upload_id']=begin(self.store,key);d['phase']='uploading';atomic(p,d)
   try:parts=list_parts(self.store,key,d['upload_id'])
   except exchange.ExchangeError as ex:
    # A lost Complete response may have already published the exact object.
    if str(ex)=='exchange_s3_http_404':
     self.store.verify(key,size,sha);d['phase']='complete';atomic(p,d);return {'complete':True}
    raise
   count=(size+PART-1)//PART
   return {'upload_id':d['upload_id'],'part_bytes':PART,'parts':[{'number':n,'size':min(PART,size-(n-1)*PART),'remote':parts.get(n),'put_url':self.store.url('PUT',key,query={'uploadId':d['upload_id'],'partNumber':str(n)})} for n in range(1,count+1)]}
 def finish(self,key,size,sha):
  with self.locked(key,size,sha) as (p,d):
   if d['phase']=='complete':self.store.verify(key,size,sha);return
   try:parts=list_parts(self.store,key,d['upload_id'])
   except exchange.ExchangeError as ex:
    if str(ex)=='exchange_s3_http_404':self.store.verify(key,size,sha);d['phase']='complete';atomic(p,d);return
    raise
   count=(size+PART-1)//PART
   if set(parts)!=set(range(1,count+1)) or any(parts[n]['size']!=min(PART,size-(n-1)*PART) for n in parts):raise exchange.ExchangeError('exchange_multipart_incomplete')
   d['phase']='completing';atomic(p,d);complete(self.store,key,d['upload_id'],parts);self.store.verify(key,size,sha);d['phase']='complete';atomic(p,d)

def send_parts(stream,plan,checkpoint,request):
 """Caller supplies bounded signed PUT; durable local SHA256/ETag match is required to skip a part."""
 if plan.get('complete'):return {'resumed_parts':0}
 p=Path(checkpoint);d=exchange.protected(p) if p.exists() else {'upload_id':plan['upload_id'],'parts':{}}
 if d['upload_id']!=plan['upload_id']:raise exchange.ExchangeError('exchange_multipart_checkpoint_upload_mismatch')
 resumed=0
 for part in plan['parts']:
  number=part['number'];stream.seek((number-1)*plan['part_bytes']);raw=stream.read(part['size']);sha=hashlib.sha256(raw).hexdigest()
  if len(raw)!=part['size']:raise exchange.ExchangeError('exchange_multipart_source_short')
  previous=d['parts'].get(str(number));remote=part.get('remote')
  if previous and remote and previous=={'sha256':sha,'etag':remote['etag'],'size':remote['size']} and len(raw)==remote['size']:resumed+=1;continue
  etag=request(part['put_url'],raw)
  if not etag or len(etag)>256:raise exchange.ExchangeError('exchange_multipart_part_etag')
  d['parts'][str(number)]={'sha256':sha,'etag':etag,'size':len(raw)};atomic(p,d)
 return {'resumed_parts':resumed}
