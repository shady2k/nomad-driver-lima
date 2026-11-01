"""Complete retained native task-log snapshot through existing Nomad read-logs API.
Bounded/redacted, no credentials, environment, guest filesystem scan or new ACL.
"""
import base64,hashlib,json,os,pathlib,re,selectors,subprocess,tempfile,time,urllib.parse
from secure_logs import Redactor

SAFE_FAILURE_CODES=frozenset('''DIAGNOSTIC_READ_TIMEOUT_VM_RETAINED DIAGNOSTIC_PROTOCOL_QUOTA_VM_RETAINED DIAGNOSTIC_FRAME_QUOTA DIAGNOSTIC_READ_UNAVAILABLE_OR_INCOMPLETE_VM_RETAINED DIAGNOSTIC_QUOTA_VM_RETAINED DIAGNOSTIC_FRAME_INVALID DIAGNOSTIC_HISTORY_CHANGED_VM_RETAINED DIAGNOSTIC_ERROR_FRAME DIAGNOSTIC_FILE_BINDING_MISMATCH DIAGNOSTIC_HISTORY_ROTATED_VM_RETAINED DIAGNOSTIC_ROTATION_GAP_VM_RETAINED DIAGNOSTIC_OFFSET_GAP_VM_RETAINED DIAGNOSTIC_INDEPENDENT_READ_MISMATCH_VM_RETAINED DIAGNOSTIC_LINE_QUOTA_VM_RETAINED DIAGNOSTIC_ATTEMPT_MISMATCH DIAGNOSTIC_PATH_MISMATCH DIAGNOSTIC_SNAPSHOT_CHANGED DIAGNOSTIC_ATTEMPT_CHANGED EXEC_LOG_PATH_MISMATCH EXEC_LOG_ATTEMPT_MISMATCH OPERATION_EXPORT_NOT_COMPLETE OPERATION_EXPORT_LOG_BOUND OPERATION_EXPORT_LOG_GAP OPERATION_EXPORT_CHECKSUM_CHANGED OPERATION_TRANSPORT_OUTCOME_UNKNOWN_NO_REPLAY OPERATION_BINDING_MISMATCH MACHINE_BOOT_CHANGED_OPERATION_UNKNOWN'''.split())
class CaptureFailure(RuntimeError):
 def __init__(self,stage,cause):
  self.stage=stage if stage in ('operations','stdout','stderr','capture') else 'capture'
  raw=str(cause)
  self.code=raw if raw in SAFE_FAILURE_CODES else {PermissionError:'PERMISSION_DENIED',FileNotFoundError:'FILE_NOT_FOUND',TimeoutError:'TIMEOUT',ValueError:'INVALID_DATA',UnicodeError:'INVALID_ENCODING'}.get(type(cause),'UNCLASSIFIED_FAILURE')
  self.error_type=type(cause).__name__ if re.fullmatch('[A-Za-z]{1,60}',type(cause).__name__) else 'Exception'
  super().__init__('DIAGNOSTIC_EXPORT_INCOMPLETE_VM_RETAINED:'+self.stage+':'+self.code+':'+self.error_type)
def failure(cause):
 return cause if isinstance(cause,CaptureFailure) else CaptureFailure('capture',cause)

def encoded(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def sha(v):return hashlib.sha256(v).hexdigest()
def frames(binary,path,limit,timeout=60):
 p=subprocess.Popen([binary,'operator','api','-X','GET',path],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
 selector=selectors.DefaultSelector();selector.register(p.stdout,selectors.EVENT_READ);pending='';decoder=json.JSONDecoder();total=0;deadline=time.monotonic()+timeout
 try:
  while True:
   if time.monotonic()>deadline:raise RuntimeError('DIAGNOSTIC_READ_TIMEOUT_VM_RETAINED')
   if not selector.select(timeout=min(1,max(0,deadline-time.monotonic()))):continue
   raw=os.read(p.stdout.fileno(),65536)
   if not raw:break
   total+=len(raw)
   if total>limit*2+4194304:raise RuntimeError('DIAGNOSTIC_PROTOCOL_QUOTA_VM_RETAINED')
   pending+=raw.decode('utf-8')
   while pending.strip():
    pending=pending.lstrip()
    try:v,end=decoder.raw_decode(pending)
    except json.JSONDecodeError:
     if len(pending)>2097152:raise RuntimeError('DIAGNOSTIC_FRAME_QUOTA')
     break
    pending=pending[end:];yield v
  if pending.strip() or p.wait(timeout=5)!=0:raise RuntimeError('DIAGNOSTIC_READ_UNAVAILABLE_OR_INCOMPLETE_VM_RETAINED')
 finally:
  selector.close();p.stdout.close()
  if p.poll() is None:p.terminate();p.wait(timeout=5)

def plain_bytes(binary,path,limit,timeout=60):
 p=subprocess.Popen([binary,'operator','api','-X','GET',path],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
 sel=selectors.DefaultSelector();sel.register(p.stdout,selectors.EVENT_READ);data=bytearray();deadline=time.monotonic()+timeout
 try:
  while True:
   if time.monotonic()>=deadline:raise RuntimeError('DIAGNOSTIC_READ_TIMEOUT_VM_RETAINED')
   if not sel.select(timeout=.2):continue
   raw=os.read(p.stdout.fileno(),65536)
   if not raw:break
   data.extend(raw)
   if len(data)>limit:raise RuntimeError('DIAGNOSTIC_QUOTA_VM_RETAINED')
  if p.wait(timeout=5):raise RuntimeError('DIAGNOSTIC_READ_UNAVAILABLE_OR_INCOMPLETE_VM_RETAINED')
  return bytes(data)
 finally:
  sel.close();p.stdout.close()
  if p.poll() is None:p.terminate();p.wait(timeout=5)

def verified_frames(records,plain,max_bytes):
 # Nomad 2.0.7's framer can retain the nominal full-frame end offset on a
 # short final frame. Accept ONLY that shape, and independently compare all
 # captured bytes with the plain read-logs stream before normalizing metadata.
 rows=list(records);captured=bytearray();previous=None
 for index,frame in enumerate(rows):
  if not isinstance(frame,dict) or not frame.get('Data'):continue
  raw=base64.b64decode(frame['Data'],validate=True);captured.extend(raw)
  if len(captured)>max_bytes:raise RuntimeError('DIAGNOSTIC_QUOTA_VM_RETAINED')
  current_file=frame.get('File');end=frame.get('Offset',0)
  start=previous['actual_end'] if previous and previous['file']==current_file else 0
  actual_end=start+len(raw)
  if end!=actual_end:
   later=next((v for v in rows[index+1:] if isinstance(v,dict) and v.get('Data')),None)
   short_tail=(previous and previous['file']==current_file and len(raw)<previous['length'] and end==previous['advertised_end']+previous['length'] and (later is None or later.get('File')!=current_file))
   if not short_tail:raise RuntimeError('DIAGNOSTIC_OFFSET_GAP_VM_RETAINED')
   rows[index]={**frame,'Offset':actual_end}
  previous={'file':current_file,'actual_end':actual_end,'advertised_end':end,'length':len(raw)}
 if len(plain)<len(captured) or plain[:len(captured)]!=captured:raise RuntimeError('DIAGNOSTIC_INDEPENDENT_READ_MISMATCH_VM_RETAINED')
 return rows

def write_file(output,name,raw):
 p=output/name;fd,tmp=tempfile.mkstemp(prefix='.diagnostic-',dir=output)
 try:
  with os.fdopen(fd,'wb') as f:os.fchmod(f.fileno(),0o600);f.write(raw);f.flush();os.fsync(f.fileno())
  os.replace(tmp,p)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
 return {'path':'.nomad-logs/'+name,'size':len(raw),'sha256':sha(raw),'local_file':name,'source':'native_diagnostic'}

def stream_snapshot(records,task,stream,output,max_bytes,redactor):
 # File/offset continuity detects lost rotation and protocol truncation. Never tail.
 name='native-'+stream+'.log';fd,tmp=tempfile.mkstemp(prefix='.diagnostic-',dir=output);h=hashlib.sha256();n=raw_total=0;pending=b'';file_index=None;offset=0;coverage=[]
 try:
  with os.fdopen(fd,'wb') as out:
   os.fchmod(out.fileno(),0o600)
   def emit(line):
    nonlocal n
    data=redactor.text(line.decode('utf-8',errors='replace')).encode();n+=len(data)
    if n>max_bytes:raise RuntimeError('DIAGNOSTIC_QUOTA_VM_RETAINED')
    out.write(data);h.update(data)
   for frame in records:
    if not isinstance(frame,dict):raise RuntimeError('DIAGNOSTIC_FRAME_INVALID')
    if frame.get('FileEvent') in ('file deleted','file truncated','delete','truncate'):raise RuntimeError('DIAGNOSTIC_HISTORY_CHANGED_VM_RETAINED')
    if set(frame)-{'Data','Offset','File','FileEvent'}:raise RuntimeError('DIAGNOSTIC_ERROR_FRAME')
    if not frame.get('Data'):continue
    m=re.fullmatch(r'(?:alloc/logs/)?'+re.escape(task)+r'\.'+stream+r'\.(\d+)',frame.get('File',''))
    if not m:raise RuntimeError('DIAGNOSTIC_FILE_BINDING_MISMATCH')
    index=int(m[1]);raw=base64.b64decode(frame['Data'],validate=True);start=frame.get('Offset',0)-len(raw) # Nomad frame Offset is END offset
    if file_index is None:
     if index!=0 or start!=0:raise RuntimeError('DIAGNOSTIC_HISTORY_ROTATED_VM_RETAINED')
     file_index=index;coverage.append({'file':frame['File'],'start':0,'end':0})
    elif index!=file_index:
     if index!=file_index+1 or start!=0:raise RuntimeError('DIAGNOSTIC_ROTATION_GAP_VM_RETAINED')
     file_index=index;offset=0;coverage.append({'file':frame['File'],'start':0,'end':0})
    if start!=offset:raise RuntimeError('DIAGNOSTIC_OFFSET_GAP_VM_RETAINED')
    offset+=len(raw);raw_total+=len(raw);coverage[-1]['end']=offset
    if raw_total>max_bytes:raise RuntimeError('DIAGNOSTIC_QUOTA_VM_RETAINED')
    pending+=raw
    while b'\n' in pending:
     line,pending=pending.split(b'\n',1);emit(line+b'\n')
    if len(pending)>1048576:raise RuntimeError('DIAGNOSTIC_LINE_QUOTA_VM_RETAINED')
   if pending:emit(pending)
   out.flush();os.fsync(out.fileno())
  os.replace(tmp,output/name)
  return {'path':'.nomad-logs/'+name,'size':n,'sha256':h.hexdigest(),'local_file':name,'source':'native_diagnostic'},coverage
 finally:
  if os.path.exists(tmp):os.unlink(tmp)

def capture(adapter,session,output):
 output=pathlib.Path(output);index=output/'diagnostics-index.json';attempt=adapter.load(session)['attempt'];max_bytes=adapter.c.get('max_diagnostic_bytes',67108864)
 if type(max_bytes)!=int or not 1048576<=max_bytes<=268435456:raise ValueError('bounded max_diagnostic_bytes required')
 if index.exists():
  doc=json.loads(index.read_text())
  if doc.get('attempt')!=attempt:raise RuntimeError('DIAGNOSTIC_ATTEMPT_MISMATCH')
  for f in doc['files']:
   p=output/f['local_file']
   if p.is_symlink() or p.parent!=output:raise RuntimeError('DIAGNOSTIC_PATH_MISMATCH')
   raw=p.read_bytes()
   if len(raw)!=f['size'] or sha(raw)!=f['sha256']:raise RuntimeError('DIAGNOSTIC_SNAPSHOT_CHANGED')
  return doc['files']+[{'path':'.nomad-logs/diagnostics-index.json','size':index.stat().st_size,'sha256':sha(index.read_bytes()),'local_file':index.name,'source':'native_diagnostic'}]
 before=adapter.target()
 if adapter.attempt(before)!=attempt:raise RuntimeError('DIAGNOSTIC_ATTEMPT_CHANGED')
 redactor=Redactor();items=[];coverage={}
 import operations
 try:items.extend(operations.capture(adapter,session,output))
 except Exception as e:raise CaptureFailure('operations',e) from None
 for stream in ('stdout','stderr'):
  query=urllib.parse.urlencode({'namespace':adapter.c['namespace'],'task':adapter.c['task'],'type':stream,'origin':'start','offset':0,'follow':'false','plain':'false'})
  path='/v1/client/fs/logs/'+adapter.c['allocation']+'?'+query
  try:
   records=list(frames(adapter.c.get('nomad_bin','nomad'),path,max_bytes))
   plain_query=urllib.parse.urlencode({'namespace':adapter.c['namespace'],'task':adapter.c['task'],'type':stream,'origin':'start','offset':0,'follow':'false','plain':'true'})
   independent=plain_bytes(adapter.c.get('nomad_bin','nomad'),'/v1/client/fs/logs/'+adapter.c['allocation']+'?'+plain_query,max_bytes)
   item,covered=stream_snapshot(verified_frames(records,independent,max_bytes),adapter.c['task'],stream,output,max_bytes,redactor)
  except Exception as e:raise CaptureFailure(stream,e) from None
  items.append(item);coverage[stream]=covered
 events=[]
 for e in before.get('TaskStates',{}).get(adapter.c['task'],{}).get('Events',[]):events.append({k:e[k] for k in ('Type','Time','ExitCode','DisplayMessage','Message','FailsTask') if k in e})
 for p in sorted(adapter.local(session).glob('exec-*.json')):
  if p.is_symlink():raise RuntimeError('EXEC_LOG_PATH_MISMATCH')
  row=json.loads(p.read_text())
  if row.get('attempt')!=attempt:raise RuntimeError('EXEC_LOG_ATTEMPT_MISMATCH')
  items.append(write_file(output,p.name,redactor.text(encoded(row).decode()).encode()))
 event_raw=redactor.text(encoded({'attempt':attempt,'task_events':events}).decode()).encode();items.append(write_file(output,'task-events.json',event_raw))
 if adapter.attempt(adapter.target())!=attempt:raise RuntimeError('DIAGNOSTIC_ATTEMPT_CHANGED')
 doc={'version':1,'session_id':session,'attempt':attempt,'scope':'all_native_stdout_stderr_from_file_zero_through_capture','activity_revision':__import__('activity').revision(adapter,session),'cut_at':time.time(),'coverage':coverage,'task_events_scope':'retained_Nomad_TaskEvents','files':items,'redacted':True,'post_commit_events_not_in_snapshot':True}
 raw=encoded(doc);write_file(output,index.name,raw)
 return items+[{'path':'.nomad-logs/diagnostics-index.json','size':len(raw),'sha256':sha(raw),'local_file':index.name,'source':'native_diagnostic'}]
