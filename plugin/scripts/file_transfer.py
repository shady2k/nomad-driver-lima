"""Owned VM input upload, existing trusted S3 exchange, no code in argv."""
import os,pathlib,hashlib,json,uuid,urllib.request,contextlib,time
from exchange import protected
from workerctl import atomic,canonical
from upload_guest import relative,PART,MAX_FILE

def fingerprint(path):
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb')as stream:
  before=os.fstat(stream.fileno())
  import stat
  if not stat.S_ISREG(before.st_mode)or not 0<=before.st_size<=MAX_FILE:raise ValueError('UPLOAD_REGULAR_FILE_MAX_256_MIB_REQUIRED')
  h=hashlib.sha256();size=0
  while True:
   chunk=stream.read(PART)
   if not chunk:break
   size+=len(chunk)
   if size>before.st_size or size>MAX_FILE:raise RuntimeError('UPLOAD_LOCAL_SOURCE_GREW_BEYOND_BOUND')
   h.update(chunk)
  after=os.fstat(stream.fileno())
  if (before.st_dev,before.st_ino,before.st_size,before.st_mtime_ns)!=(after.st_dev,after.st_ino,after.st_size,after.st_mtime_ns)or size!=before.st_size:raise RuntimeError('UPLOAD_LOCAL_SOURCE_CHANGED')
  return {'size':size,'sha256':h.hexdigest(),'dev':before.st_dev,'ino':before.st_ino,'mtime_ns':before.st_mtime_ns}

def source_snapshot(source_path,source,folder):
 target=folder/'source.bin'
 if target.exists():
  if fingerprint(target)['sha256']!=source['sha256']:raise RuntimeError('UPLOAD_SOURCE_SNAPSHOT_CHANGED')
  return target
 fd=os.open(source_path,os.O_RDONLY|os.O_NOFOLLOW);tmp=folder/('source-'+str(uuid.uuid4())+'.partial')
 try:
  with os.fdopen(fd,'rb')as original:
   st=os.fstat(original.fileno())
   if (st.st_dev,st.st_ino,st.st_size,st.st_mtime_ns)!=(source['dev'],source['ino'],source['size'],source['mtime_ns']):raise RuntimeError('UPLOAD_LOCAL_SOURCE_CHANGED')
   dest=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600);h=hashlib.sha256();size=0
   with os.fdopen(dest,'wb')as output:
    while True:
     chunk=original.read(PART)
     if not chunk:break
     size+=len(chunk)
     if size>source['size']or size>MAX_FILE:raise RuntimeError('UPLOAD_LOCAL_SOURCE_GREW_BEYOND_BOUND')
     h.update(chunk);output.write(chunk)
    output.flush();os.fsync(output.fileno())
   after=os.fstat(original.fileno())
   if size!=source['size']or h.hexdigest()!=source['sha256']or(st.st_dev,st.st_ino,st.st_size,st.st_mtime_ns)!=(after.st_dev,after.st_ino,after.st_size,after.st_mtime_ns):raise RuntimeError('UPLOAD_LOCAL_SOURCE_CHANGED')
  os.link(tmp,target,follow_symlinks=False);os.unlink(tmp);return target
 finally:
  if tmp.exists():tmp.unlink()

def rpc(a,s,row,action,**fields):
 a.require_initiator(s);state=a.load(s);live=a.target()
 from exec_checkpoint import verify_owner
 verify_owner(a,s,live)
 if state['attempt']!=a.attempt(live)or row['attempt']!=state['attempt']:raise RuntimeError('UPLOAD_ATTEMPT_CHANGED')
 payload={'config':a.c['guest'],'session_id':s,'upload_id':row['operation_id'],'binding':row['binding'],'size':row['source']['size'],'sha256':row['source']['sha256'],'destination':row['destination'],'extract_to':row['extract_to'],'action':action,**fields}
 if row.get('machine_identity'):payload['machine_identity']=row['machine_identity']
 program=pathlib.Path(__file__).with_name('upload_guest.py').read_text()+"\np=json.load(sys.stdin)\ntry:print(json.dumps({'ok':True,'result':handle(p)}))\nexcept Exception as e:\n code=str(e)\n print(json.dumps({'ok':False,'code':code if __import__('re').fullmatch('[A-Z_]{1,100}',code)else'UPLOAD_GUEST_UNAVAILABLE'}))\n"
 args=['alloc','exec','-namespace='+a.c['namespace'],'-task='+a.c['task'],'-i=true','-t=false',a.c['allocation'],'/usr/bin/python3','-c',program]
 try:
  result=a.nomad(args,input=json.dumps(payload),text=True,timeout=40)
  if len(result.stdout)>65536:raise RuntimeError('UPLOAD_RESPONSE_BOUND')
  value=json.loads(result.stdout)
 except (ValueError,OSError,TimeoutError):raise RuntimeError('UPLOAD_TRANSPORT_UNKNOWN_RETRY_SAME_FILE_NO_WORKLOAD_REPLAY')from None
 if not value.get('ok'):raise RuntimeError(value.get('code','UPLOAD_REMOTE_UNAVAILABLE'))
 if result.returncode:raise RuntimeError('UPLOAD_TRANSPORT_UNKNOWN_RETRY_SAME_FILE_NO_WORKLOAD_REPLAY')
 out=value['result']
 if out.get('size')!=row['source']['size']or out.get('sha256')!=row['source']['sha256']or(row.get('machine_identity')and out.get('machine_identity')!=row['machine_identity']):raise RuntimeError('UPLOAD_RESPONSE_BINDING_CHANGED')
 return out

def upload(service,vm,local,destination,extract_to=None):
 import activity,exec_checkpoint,storage,exchange,multipart
 from vm_interface import private_dir,private_lock
 relative(destination)
 if extract_to:relative(extract_to)
 source_path=pathlib.Path(local).absolute();source=fingerprint(source_path);a,_=service.resolve(vm)
 if a is None:raise RuntimeError('UPLOAD_VERIFIED_VM_BINDING_REQUIRED')
 with a.lock(vm):
  state=exec_checkpoint.ensure_session(a,vm);exec_checkpoint.verify_owner(a,vm,a.target())
  if state.get('phase')not in ('ready','management_ready')or state.get('closing_candidate'):raise RuntimeError('UPLOAD_VM_FINISHING')
  settings=a.c.get('storage')or{}
  if settings.get('backend')!='s3':raise RuntimeError('UPLOAD_EXISTING_S3_EXCHANGE_REQUIRED_NO_NEW_GRANT')
  binding={'session_id':vm,'attempt':state['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
  writer,backend=storage.store(settings,True);reader,reader_backend=storage.store(settings)
  if backend!=reader_backend:raise RuntimeError('UPLOAD_READER_WRITER_BACKEND_MISMATCH')
  key=hashlib.sha256(canonical({'binding':binding,'sha256':source['sha256'],'size':source['size'],'destination':destination,'extract_to':extract_to})).hexdigest();identifier=str(uuid.UUID(key[:32]));path=a.local(vm)/('upload-'+identifier+'.json')
  if path.exists():
   row=protected(path)
   if row.get('abort_requested')or row.get('phase')=='upload_aborted':raise RuntimeError('UPLOAD_ABORT_INTENT_NO_SOURCE_REPLAY')
   if row.get('binding')!=binding or row.get('source',{}).get('sha256')!=source['sha256']:raise RuntimeError('UPLOAD_BINDING_CHANGED')
   activity.pending(a,vm,exclude=path)
  else:
   activity.pending(a,vm);row={'operation_id':identifier,'work_kind':'upload','phase':'unknown','source':source,'destination':destination,'extract_to':extract_to,'binding':binding,'attempt':state['attempt'],'started_at':time.time()};activity.record(a,vm,path,row)
  prefix=exchange.identity(a.c['job'],vm,identifier);object_key=prefix+'objects/'+source['sha256'];folder=private_dir(a.local(vm)/('upload-state-'+identifier));staged=source_snapshot(source_path,source,folder)
  try:
   exists=writer.stat(object_key)
  except exchange.ExchangeError as error:
   if str(error)!='exchange_s3_http_404':raise
   exists=None
  if exists is None:
   if source['size']>=PART:
    manager=multipart.Manager(writer,folder/'multipart');plan=manager.plan(object_key,source['size'],source['sha256'])
    def put(url,body):
     request=urllib.request.Request(url,data=body,method='PUT')
     with urllib.request.build_opener(exchange.NoRedirect).open(request,timeout=30)as response:return response.headers.get('ETag')
    with staged.open('rb')as stream:multipart.send_parts(stream,plan,folder/'parts.json',put)
    if fingerprint(staged)['sha256']!=source['sha256']:raise RuntimeError('UPLOAD_SOURCE_SNAPSHOT_CHANGED')
    manager.finish(object_key,source['size'],source['sha256'])
   else:
    with staged.open('rb')as stream:raw=stream.read(PART)
    if len(raw)!=source['size']or hashlib.sha256(raw).hexdigest()!=source['sha256']:raise RuntimeError('UPLOAD_LOCAL_SOURCE_CHANGED')
    writer.conditional_put(object_key,raw)
  writer.verify(object_key,source['size'],source['sha256']);reader.verify(object_key,source['size'],source['sha256'])
  current=rpc(a,vm,row,'status');row['machine_identity']=current['machine_identity'];atomic(path,row)
  with staged.open('rb')as stream:
   for _ in range(32):
    offset=current['offset']
    if offset==source['size']:break
    if type(offset)is not int or offset<0 or offset>source['size']:raise RuntimeError('UPLOAD_REMOTE_OFFSET_INVALID')
    stream.seek(offset);body=stream.read(min(PART,source['size']-offset));url=reader.url('GET',object_key)
    current=rpc(a,vm,row,'part',offset=offset,part_size=len(body),part_sha256=hashlib.sha256(body).hexdigest(),url=url,origin=reader.cfg['endpoint']);row['offset']=current['offset'];atomic(path,row)
   else:
    if current['offset']!=source['size']:raise RuntimeError('UPLOAD_PART_COUNT_BOUND')
  final=rpc(a,vm,row,'finish')
  if final['phase']!='completed'or final['offset']!=source['size']or bool(final['extract_verified'])!=bool(extract_to):raise RuntimeError('UPLOAD_FINALIZATION_UNCONFIRMED')
  staged.unlink()
  row.update(phase='upload_completed',offset=source['size'],finished_at=time.time(),sha256_verified=True,extract_verified=bool(extract_to));atomic(path,row)
  return {'vm_id':vm,'upload_id':identifier,'state':'uploaded','destination':destination,'size_bytes':source['size'],'sha256':source['sha256'],'extract_to':extract_to,'extract_verified':bool(extract_to),'model_requests':0,'transport':'existing-s3-exchange-and-native-exec','static_credentials_sent_to_guest':False}

def control(service,vm,identifier=None,action='status'):
 """Exact existing transfer lookup/reconciliation; never resend source/workload."""
 from workerctl import uid
 from exec_checkpoint import verify_owner
 if identifier is not None:uid(identifier)
 if action not in ('status','reconcile','abort'):raise ValueError('UPLOAD_CONTROL_ACTION_INVALID')
 a,_=service.resolve(vm)
 if a is None:raise RuntimeError('UPLOAD_VERIFIED_VM_BINDING_REQUIRED')
 with (contextlib.nullcontext()if action=='status'else a.lock(vm)):
  verify_owner(a,vm,a.target())
  if action!='status'and(a.load(vm).get('phase')not in ('ready','management_ready')or a.load(vm).get('closing_candidate')):raise RuntimeError('UPLOAD_VM_FINISHING')
  if identifier is None:
   paths=list(a.local(vm).glob('upload-*.json'))
   if len(paths)>128:raise RuntimeError('UPLOAD_DISCOVERY_LIMIT')
   rows=[protected(p)for p in paths];active=[r for r in rows if r.get('phase')not in ('upload_completed','upload_aborted')]
   if len(active)==1:chosen=active[0]
   elif len(active)>1:raise RuntimeError('UPLOAD_SELECTION_AMBIGUOUS')
   elif len(rows)==1:chosen=rows[0]
   elif rows and all(type(r.get('activity_revision'))is int for r in rows)and len({r['activity_revision']for r in rows})==len(rows):chosen=max(rows,key=lambda r:r['activity_revision'])
   else:raise RuntimeError('UPLOAD_SELECTION_MISSING_OR_AMBIGUOUS')
   identifier=uid(chosen['operation_id'])
  path=a.local(vm)/('upload-'+identifier+'.json');row=protected(path)
  if row.get('work_kind')!='upload'or row.get('operation_id')!=identifier:raise RuntimeError('UPLOAD_LOCAL_BINDING_CHANGED')
  if action=='abort':
   row['abort_requested']=True;atomic(path,row)
  observed=rpc(a,vm,row,'abort'if action=='abort'or(action=='reconcile'and row.get('abort_requested'))else'inspect')
  if action=='reconcile'and observed['phase']=='abort_pending':
   observed=rpc(a,vm,row,'abort')
  if action=='reconcile'and observed['phase']in ('receiving','completed')and observed['offset']==row['source']['size']and(observed['phase']=='receiving'or(row['extract_to']and not observed.get('extract_verified'))):
   observed=rpc(a,vm,row,'finish') # finalize only verified existing bytes; no part/source replay
  terminal=observed['phase']=='aborted'or(observed['phase']=='completed'and bool(observed.get('extract_verified'))==bool(row['extract_to']))
  if action!='status'and terminal:
   row.update(phase='upload_aborted'if observed['phase']=='aborted'else'upload_completed',machine_identity=observed['machine_identity'],offset=observed['offset'],reconciled_at=time.time());atomic(path,row)
  return {'vm_id':vm,'upload_id':identifier,'state':row['phase'],'guest_state':observed['phase'],'offset_bytes':observed['offset'],'total_bytes':row['source']['size'],'verified_terminal':terminal,'read_only':action=='status','source_replayed':False,'published_outputs_retained':True,'exchange_objects_retained':True,'storage_cleanup':'not_requested_existing_exchange_retention','next_action':'none'if terminal else'resume_same_upload_or_abort_exact_upload'}
