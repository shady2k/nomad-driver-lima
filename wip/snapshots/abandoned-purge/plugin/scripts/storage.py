"""Reuse the existing exchange commit protocol; imports/secrets are lazy when off.
No service, new credentials, guest writer key, or silent S3 fallback.
"""
import hashlib,json,pathlib

def binding(cfg):
 return hashlib.sha256(json.dumps({k:cfg[k] for k in ('endpoint','bucket','region')},sort_keys=True,separators=(',',':')).encode()).hexdigest()
def store(settings,write=False):
 import exchange
 key='writer_config' if write else 'reader_config'
 if not settings.get(key):raise RuntimeError('S3_TRUSTED_WRITER_NOT_CONFIGURED' if write else 'S3_READER_NOT_CONFIGURED')
 cfg=exchange.config(pathlib.Path(settings[key]));return exchange.Store(cfg),binding(cfg)
def commit(settings,job,session,request,files,output,attempt_binding):
 # Only an explicitly approved trusted coordinator/host may configure writer_config.
 # Workers never receive this config or keys. Missing writer is an explicit blocker.
 import exchange,multipart
 writer,backend=store(settings,True);reader,read_backend=store(settings)
 if backend!=read_backend:raise RuntimeError('S3_READER_WRITER_BACKEND_MISMATCH')
 output=pathlib.Path(output)
 binding_doc={'version':1,'job':job,'session_id':session,'request_id':request,'attempt':attempt_binding,'files':exchange.files(files)}
 raw=exchange.canonical(binding_doc);terminal_path=output/'terminal-binding.json';terminal_path.write_bytes(raw);terminal_path.chmod(0o600)
 binding_item={'path':'.nomad-results/'+request+'.json','size':len(raw),'sha256':hashlib.sha256(raw).hexdigest(),'local_file':terminal_path.name}
 files=[*files,binding_item];fs=exchange.files(files);prefix=exchange.identity(job,session,request)
 for item in fs:
  src=pathlib.Path(output)/next(f['local_file'] for f in files if f['path']==item['path'])
  raw=src.read_bytes()
  if len(raw)!=item['size'] or hashlib.sha256(raw).hexdigest()!=item['sha256']:raise RuntimeError('S3_SOURCE_CHANGED')
  # Existing multipart implementation persists no URLs/signing secrets.
  if len(raw)>=multipart.PART:
   key=prefix+'objects/'+item['sha256'];manager=multipart.Manager(writer,output/'multipart-manager');plan=manager.plan(key,item['size'],item['sha256'])
   def put_part(url,body):
    import urllib.request
    req=urllib.request.Request(url,method='PUT',data=body)
    with urllib.request.build_opener(exchange.NoRedirect).open(req,timeout=30) as response:return response.headers.get('ETag')
   with src.open('rb') as stream:multipart.send_parts(stream,plan,output/('parts-'+item['sha256']+'.json'),put_part)
   manager.finish(key,item['size'],item['sha256'])
  else:writer.put(prefix+'objects/'+item['sha256'],raw)
 receipt=writer.commit(job,session,request,fs)
 verify(settings,receipt,backend,reader=reader,attempt_binding=attempt_binding)
 return {'actual_backend':'s3','backend_binding':backend,'receipt':receipt,'export_binding':attempt_binding,'durability':'verified_object_store_commit_not_independent_backup'}
def verify(settings,receipt,backend,reader=None,attempt_binding=None):
 if reader is None:reader,actual=store(settings)
 else:
  import exchange
  actual=binding(reader.cfg)
 if actual!=backend:raise RuntimeError('RESULT_RECEIPT_BACKEND_MISMATCH')
 doc=reader.manifest(receipt['job'],receipt['session_id'],receipt['request_id'],receipt['manifest_sha256'])
 if doc.get('scope')!='declared-terminal-results':raise RuntimeError('S3_RECEIPT_SCOPE')
 prefix=__import__('exchange').identity(receipt['job'],receipt['session_id'],receipt['request_id'])
 for item in doc['files']:reader.verify(prefix+'objects/'+item['sha256'],item['size'],item['sha256'])
 terminal=[x for x in doc['files'] if x['path']=='.nomad-results/'+receipt['request_id']+'.json']
 if len(terminal)!=1:raise RuntimeError('S3_TERMINAL_BINDING_MISSING')
 b=json.loads(reader.get(prefix+'objects/'+terminal[0]['sha256']))
 expected_files=__import__('exchange').files([x for x in doc['files'] if x['path']!='.nomad-results/'+receipt['request_id']+'.json'])
 if b.get('files')!=expected_files:raise RuntimeError('S3_FINAL_MANIFEST_BINDING_MISMATCH')
 if attempt_binding and attempt_binding.get('diagnostics_required'):
  entries=[f for f in doc['files'] if f['path']=='.nomad-logs/diagnostics-index.json']
  if len(entries)!=1:raise RuntimeError('S3_DIAGNOSTIC_MANIFEST_MISSING')
  idx=json.loads(reader.get(prefix+'objects/'+entries[0]['sha256']))
  expected_attempt={k:v for k,v in attempt_binding.items() if k not in ('acpx_request_id','terminal','diagnostics_required','checkpoint')}
  if idx.get('attempt')!=expected_attempt or idx.get('session_id')!=receipt['session_id'] or idx.get('scope')!='all_native_stdout_stderr_from_file_zero_through_capture':raise RuntimeError('S3_DIAGNOSTIC_BINDING_MISMATCH')
  cp=attempt_binding.get('checkpoint')
  if cp is not None and idx.get('activity_revision')!=cp['activity_revision']:raise RuntimeError('S3_DIAGNOSTIC_ACTIVITY_MISMATCH')
  by_path={f['path']:f for f in doc['files']}
  for f in idx['files']:
   if f['path'] not in by_path or any(by_path[f['path']][k]!=f[k] for k in ('size','sha256')):raise RuntimeError('S3_DIAGNOSTIC_FILE_MISSING')
 if (b.get('job'),b.get('session_id'),b.get('request_id'),b.get('attempt'))!=(receipt['job'],receipt['session_id'],receipt['request_id'],attempt_binding):raise RuntimeError('S3_ATTEMPT_BINDING_MISMATCH')
 return doc


def restore(settings,manifest,item,output):
 """Missing local cache may be restored from its receipt backend after VM stop."""
 import exchange,os,tempfile
 reader,actual=store(settings)
 if actual!=manifest['backend_binding']:raise RuntimeError('RESULT_RECEIPT_BACKEND_MISMATCH')
 prefix=exchange.identity(manifest['receipt']['job'],manifest['session_id'],manifest['receipt']['request_id'])
 raw=reader.get(prefix+'objects/'+item['sha256'],limit=item['size'])
 if len(raw)!=item['size'] or hashlib.sha256(raw).hexdigest()!=item['sha256']:raise RuntimeError('S3_RESTORED_OBJECT_MISMATCH')
 output=pathlib.Path(output);dest=output/item['local_file']
 if dest.parent!=output or dest.is_symlink():raise RuntimeError('RESULT_PATH_MISMATCH')
 fd,tmp=tempfile.mkstemp(prefix='.readback-',dir=output)
 try:
  with os.fdopen(fd,'wb') as f:os.fchmod(f.fileno(),0o600);f.write(raw);f.flush();os.fsync(f.fileno())
  os.replace(tmp,dest)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
