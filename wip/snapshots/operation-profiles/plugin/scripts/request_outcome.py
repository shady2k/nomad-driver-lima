"""Safe native refusal summaries. Submission history is not current readiness."""
import re,urllib.parse,json
from exchange import protected
from secure_logs import Redactor

def safe(v):return Redactor().text(str(v))[:2048]
def events(full,task):
 out=[]
 for e in full.get('TaskStates',{}).get(task,{}).get('Events',[])[-32:]:
  kind=str(e.get('Type',''));details=e.get('Details')or{}
  if kind not in ('Driver Failure','Failed Validation','Setup Failure','Not Restarting','Terminated')and not e.get('FailsTask'):continue
  text=e.get('DriverError')or details.get('driver_error')or e.get('DisplayMessage')or e.get('Message')or kind
  row={'type':kind,'time':e.get('Time'),'message':safe(text),'fails_task':bool(e.get('FailsTask'))}
  if 'quota'in str(text).lower():
   row['code']='NODE_QUOTA_EXCEEDED'
   match=re.search(r'resource=(vms|vcpus|reservation_mib|disk_gib) requested=(\d+) used=(\d+) total=(\d+) limit=(\d+)',str(text))
   if match:row['quota']={'resource':match[1],**{k:int(v)for k,v in zip(('requested','used','total','limit'),match.groups()[1:])},'available':max(0,int(match[5])-int(match[3]))}
  out.append(row)
 return out[-8:]

def terminal(client,session,intent,rows,runtime):
 ns=urllib.parse.quote(client.c['namespace'],safe='');allocations=[]
 for stub in rows:
  try:full=client.api('/v1/allocation/'+stub['ID']+'?namespace='+ns)
  except RuntimeError:
   allocations.append({'id':stub['ID'],'status':stub.get('ClientStatus'),'detail_unavailable':True,'reason':'NOMAD_TERMINAL_DETAIL_UNAVAILABLE','not_proof_vm_absent':True});continue
  meta=full.get('Job',{}).get('Meta',{})
  if meta.get('nomad_workers_owner')!=client.c['owner_id']or meta.get('creator_agent_id')!=intent.get('creator_agent_id',client.c['agent_instance_id'])or meta.get('creator_session_id')!=intent.get('creator_session_id',session):raise RuntimeError('TERMINAL_ALLOCATION_OWNER_MISMATCH')
  if intent.get('creator_generation')and(meta.get('creator_generation')!=intent['creator_generation']or meta.get('worker_session_id')!=session):raise RuntimeError('TERMINAL_ALLOCATION_GENERATION_MISMATCH')
  tasks=[t for g in full.get('Job',{}).get('TaskGroups',[])for t in g.get('Tasks',[])if t.get('Name')==client.c['task']]
  requested={k:tasks[0].get('Config',{}).get(k)for k in ('guest_cpus','memory_mib','disk_gib','architecture')}if len(tasks)==1 else None
  task=full.get('TaskStates',{}).get(client.c['task'],{})
  allocations.append({'id':stub['ID'],'status':full.get('ClientStatus',stub.get('ClientStatus')),'node_id':full.get('NodeID'),'task_state':task.get('State'),'started_at':task.get('StartedAt'),'create_index':full.get('CreateIndex',stub.get('CreateIndex')),'events':events(full,client.c['task']),'requested':requested,'not_proof_vm_absent':True})
 selected=None
 binding_path=client.root/session/'target-config.json'
 if binding_path.exists():
  binding=protected(binding_path)
  if binding.get('owner_id')!=client.c['owner_id']or binding.get('job')!=intent['job']:raise RuntimeError('TERMINAL_BOUND_TARGET_MISMATCH')
  selected=next((a for a in allocations if a['id']==binding.get('allocation')),None)
 if selected is None:
  if len(allocations)==1:selected=allocations[0]
  elif all(type(a.get('create_index'))==int for a in allocations):
   latest=max(a['create_index']for a in allocations);matches=[a for a in allocations if a['create_index']==latest]
   if len(matches)==1:selected=matches[0]
 current=[selected]if selected else allocations
 statuses={a['status']for a in current};failed=bool(statuses&{'failed','lost'});quota=any(e.get('code')=='NODE_QUOTA_EXCEEDED'for a in current for e in a.get('events',[]))
 current_capacity=None;capacity_error=None
 if quota:
  try:
   nodes=client.capacity()['nodes'];ids={a.get('node_id')for a in allocations}
   current_capacity=[{k:n.get(k)for k in ('node_id','available_for_one_new_vm','occupied','node_operator_limits','placement_eligible','placement_reasons')}for n in nodes if n.get('node_id')in ids]
  except RuntimeError:capacity_error='CURRENT_CAPACITY_UNAVAILABLE'
 return {**intent,'submission_phase':intent.get('phase'),'phase':'allocation_failed'if failed else'allocation_complete','terminal':True,'failed':failed,'stop_waiting':True,'exit_code':2 if failed else 0,'allocation':None,'allocations':allocations,'current_terminal_allocation_id':selected['id']if selected else None,'terminal_history_ambiguous':selected is None and len(allocations)>1,'vm_ready':None,'runtime_ready':None,'session_ready':None,'transport_ready':None,'runtime':runtime,'readiness_stage':'allocation_terminal','reason_code':'NODE_QUOTA_EXCEEDED'if quota else'NOMAD_ALLOCATION_TERMINAL','current_capacity_advisory':current_capacity,'capacity_read_error':capacity_error,'next_action':'inspect_lost_allocation_no_new_vm'if'lost'in statuses else'adjust_resources_or_release_finished_disks_then_new_session'if quota else'inspect_terminal_events_before_new_request'if failed else'inspect_exported_results','instruction':'Stop waiting for this request. Do not resubmit the same session or replay a prompt.','prompt_retry':False,'automatic_retry':False}
