"""Work revisions and checkpoint fences. Caller holds the existing session lock."""
import json,pathlib
UNKNOWN={'rebind_unknown','binding_pending','submit_intent','submit_unknown','queued','watch_unknown','exec_unknown','start_unknown','unknown','launch_intent','launching','running'}

def records(a,s):
 paths=list(a.local(s).glob('*.json'));folder=a.local(s)/'checkpoints'
 if folder.exists():
  if folder.is_symlink():raise RuntimeError('CHECKPOINT_PATH_MISMATCH')
  paths.extend(folder.glob('*.json'))
 for p in paths:
  if p.is_symlink():raise RuntimeError('ACTIVITY_RECORD_PATH_MISMATCH')
  yield p,json.loads(p.read_text())

def revision(a,s):
 state=a.load(s);values=[state.get('activity_revision',0)]
 values.extend(row['activity_revision'] for _,row in records(a,s) if 'activity_revision' in row)
 if any(type(x)!=int or x<0 for x in values):raise RuntimeError('INVALID_ACTIVITY_REVISION')
 return max(values)

def pending(a,s,exclude=None,allow_running=False):
 for p,row in records(a,s):
  if exclude is not None and p==exclude:continue
  phase=row.get('phase')
  if (p.name.startswith('runtime-rebind-')or row.get('type')=='runtime_rebind')and(phase!='ready'or row.get('session_ready')is not True):raise RuntimeError('RUNTIME_RECOVERY_UNCONFIRMED_NO_NEW_ACTIVITY')
  if phase in ('exporting','export_failed_retryable') or (row.get('export_staging_dir') and not row.get('export_verified')):raise RuntimeError('UNVERIFIED_EXPORT_VM_RETAINED_RETRY_TRANSFER')
  work=any(k in row for k in ('request_id','execution_id','operation_id'))
  if work and phase in UNKNOWN:
   if allow_running and phase in ('queued','launching','running'):continue
   raise RuntimeError('UNFINISHED_OR_UNKNOWN_ACTIVITY_VM_RETAINED')

def record(a,s,path,row):
 from workerctl import atomic
 state=a.load(s)
 if state.get('phase')!='ready':raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
 pending(a,s,allow_running=True)
 if pathlib.Path(path).exists():raise RuntimeError('ACTIVITY_ALREADY_RECORDED_NO_REPLAY')
 row['activity_revision']=revision(a,s)+1
 # Receipt is durable BEFORE transport; recovery derives revision from receipts
 # even if the subsequent session-pointer write was interrupted.
 atomic(path,row)
 state.update(activity_revision=row['activity_revision']);state.pop('last_checkpoint',None)
 atomic(a.local(s)/'session.json',state)
 return row

def checkpoint(a,s,request,snapshot):
 return {'schema_version':1,'activity_revision':revision(a,s),'snapshot_id':snapshot,'model_request_id':request}

def require_current(a,s,manifest,request,snapshot):
 cp=manifest.get('checkpoint')
 if cp!={'schema_version':1,'activity_revision':revision(a,s),'snapshot_id':snapshot,'model_request_id':request}:raise RuntimeError('STALE_OR_LEGACY_CHECKPOINT_COLLECT_NEW_SNAPSHOT')
 return cp
