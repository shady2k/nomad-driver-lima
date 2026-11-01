"""Typed no-agent-turn checkpoint; ownership is independent of activity type."""
import json,pathlib,base64,tempfile,os,re
from exchange import protected

def project_binding(a,s,meta):
 file=a.local(s)/'allocation-intent.json'
 if file.exists():
  intent=protected(file)
  if 'project'in intent and meta.get('project','')!=intent['project']:raise RuntimeError('CHECKPOINT_PROJECT_BINDING_CHANGED')

def verify_owner(a,s,live):
 a.require_initiator(s);meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)

def local_idle(a,s):
 import activity,operations
 a.require_initiator(s);state=a.load(s);live=a.target()
 if a.attempt(live)!=state.get('attempt'):raise RuntimeError('CHECKPOINT_ATTEMPT_CHANGED')
 meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)
 activity.pending(a,s)
 for p,row in activity.records(a,s):
  if row.get('request_id'):raise RuntimeError('CHECKPOINT_AGENT_TURN_PRESENT_USE_TURN_FINISH')
  if row.get('execution_id')and row.get('phase')!='exec_completed':raise RuntimeError('CHECKPOINT_EXEC_NOT_KNOWN_TERMINAL')
 operations.verify_terminal(a,s)
 return state

def ensure_session(a,s):
 from workerctl import atomic
 a.require_initiator(s);file=a.local(s)/'session.json'
 if file.exists():return a.load(s)
 live=a.target();meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)
 state={'session_id':s,'config_hash':a.hash,'phase':'management_ready','attempt':a.attempt(live),'work_kind':'exec_only','actual_backend':'lima/nomad-exec/management-no-agent-turns'};atomic(file,state);return state

