"""VM facade contracts. Fakes never contact a scheduler, guest or model."""
import sys,pathlib,tempfile,unittest,json,uuid,os
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'scripts'))
import vm_interface as v
import session_hook as hook

def uid():return str(uuid.uuid4())
def write(p,d):p.parent.mkdir(parents=True,exist_ok=True,mode=0o700);p.write_text(json.dumps(d));p.chmod(0o600);return p
class Client:
 def __init__(self,root):self.root=root;self.c={'owner_id':uid(),'namespace':'workers','task':'worker'};self.calls=[];self.responses={}
 def api(self,path):self.calls.append(path);return self.responses[path]
 def request(self,*a,**kw):
  self.calls.append((a,kw));write(self.root/a[0]/'allocation-intent.json',{'session_id':a[0]})
 def registration_status(self,vm,allow_register=False):return {'registration':'accepted','state':'existing','vm_id':vm}
 def run(self,args,input=None):return __import__('types').SimpleNamespace(returncode=0,stdout=b'job not found')
class Tests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.root=pathlib.Path(self.tmp.name);self.client=Client(self.root/'results');self.client.root.mkdir(mode=0o700)
  self.record=write(self.root/'creator.json',{'version':1,'native_session_id':'native-test','agent_instance_id':uid(),'generation':uid(),'ended':False,'evidence':'native_hook_observation_not_authenticated_liveness','native_tool_use_id':'tool-1'})
  current=write(self.root/'current.json',json.loads(self.record.read_text()));data=json.loads(self.record.read_text());data['native_creator_record']=str(current);write(self.record,data)
  self.vm=v.VMs(self.client,str(self.record));self.id=uid()
 def job(self):return {'ID':'worker-'+self.id,'Namespace':'workers','Status':'running','Meta':{'nomad_workers_owner':self.client.c['owner_id'],'creator_agent_id':self.vm.me['agent_id'],'creator_session_id':'native-test','creator_generation':self.vm.me['generation'],'worker_session_id':self.id},'TaskGroups':[{'Tasks':[{'Name':'worker','Driver':'lima','Config':{'worker_session_id':self.id}}]}]}
 def discovery(self,allocations):
  j=self.job();self.client.responses={'/v1/jobs?namespace=workers&prefix=worker-&meta=true&per_page=1025':[j],'/v1/job/worker-'+self.id+'?namespace=workers':j,'/v1/job/worker-'+self.id+'/allocations?namespace=workers':allocations}
 def params(self):return dict(template='base',template_version='1',task_title='test',vcpus=2,memory_mib=2048,disk_gib=8)
 def test_nomad_list_without_cache(self):
  self.discovery([{'ID':uid(),'ClientStatus':'running'}]);out=self.vm.listing();self.assertEqual(out['vms'][0]['vm_id'],self.id);self.assertFalse(out['vms'][0]['local_binding_available']);self.assertTrue(out['from_nomad'])
 def test_duplicate_allocations_are_explicit(self):
  self.discovery([{'ID':uid(),'ClientStatus':'running'},{'ID':uid(),'ClientStatus':'pending'}]);self.assertTrue(self.vm.listing()['vms'][0]['ambiguous']);self.assertRaisesRegex(RuntimeError,'AMBIGUOUS',self.vm.resolve,self.id)
 def test_pending_visible_without_adoption(self):
  self.discovery([]);adapter,state=self.vm.resolve(self.id);self.assertIsNone(adapter);self.assertEqual(state['state'],'awaiting_allocation');self.assertFalse((self.client.root/self.id).exists())
 def test_other_generation_not_mine(self):
  self.discovery([]);self.job();list(self.client.responses.values())[0][0]['Meta']['creator_generation']=uid();self.assertEqual(self.vm.listing()['vms'],[])
 def test_task_identity_required(self):
  j=self.job();j['TaskGroups'][0]['Tasks'][0]['Config']['worker_session_id']=uid();self.assertFalse(self.vm.ownership(j,self.id))
 def test_create_stable_no_repeat(self):
  one=self.vm.create(self.params());two=self.vm.create(self.params());self.assertEqual(one['vm_id'],two['vm_id']);self.assertEqual(len(self.client.calls),1);self.assertNotIn('request_id',one);self.assertEqual(self.client.calls[0][1]['creator_record'],self.vm.native['native_creator_record'])
 def test_create_lost_response_not_replayed(self):
  with patch.object(self.client,'request',side_effect=RuntimeError('lost'))as req:
   one=self.vm.create(self.params());two=self.vm.create(self.params());self.assertEqual(one['state'],'not_dispatched');self.assertEqual(one['vm_id'],two['vm_id']);self.assertEqual(req.call_count,1)
 def test_call_changed_input_no_repeat(self):
  self.vm.create(self.params());params=self.params();params['disk_gib']=9;self.assertRaisesRegex(RuntimeError,'NO_REPLAY',self.vm.create,params);self.assertEqual(len(self.client.calls),1)
 def test_missing_native_call_no_dispatch(self):
  self.vm.native.pop('native_tool_use_id');self.assertRaisesRegex(RuntimeError,'NO_DISPATCH',self.vm.create,self.params());self.assertEqual(self.client.calls,[])
 def adapter(self):
  from types import SimpleNamespace
  folder=self.client.root/self.id;folder.mkdir(mode=0o700,exist_ok=True);write(folder/'session.json',{'phase':'ready','attempt':{'id':'same'}})
  return SimpleNamespace(local=lambda s:folder,load=lambda s:json.loads((folder/'session.json').read_text()))
 def turn(self,a,phase='terminal',revision=1):
  q=uid();write(a.local(self.id)/(q+'.json'),{'session_id':self.id,'request_id':q,'prompt_sha256':'0'*64,'phase':phase,'activity_revision':revision});return q
 def test_real_receipt_names_detected(self):
  a=self.adapter();q=self.turn(a);self.assertEqual(self.vm.choose(a,self.id)['request_id'],q)
 def test_sole_unknown_selected_without_replay(self):
  a=self.adapter();self.turn(a);q=self.turn(a,'watch_unknown',2);self.assertEqual(self.vm.choose(a,self.id)['request_id'],q)
 def test_multiple_unknown_no_guess(self):
  a=self.adapter();self.turn(a,'queued');self.turn(a,'watch_unknown',2);self.assertRaisesRegex(RuntimeError,'AMBIGUOUS',self.vm.choose,a,self.id)
 def test_completed_order_revision_not_filename(self):
  a=self.adapter();q=self.turn(a,revision=9);self.turn(a,revision=2);self.assertEqual(self.vm.choose(a,self.id)['request_id'],q)
 def test_duplicate_revision_rejected(self):
  a=self.adapter();self.turn(a);self.turn(a);self.assertRaisesRegex(RuntimeError,'AMBIGUOUS',self.vm.choose,a,self.id)
 def test_history_pages_hidden_and_readonly(self):
  a=self.adapter();q=self.turn(a);before=(a.local(self.id)/(q+'.json')).read_bytes();self.vm.resolve=lambda *_:(a,{})
  pages=[{'entries':[],'has_more':True,'cursor':'opaque'},{'entries':[],'has_more':False,'history_complete':True}]
  with patch('request_transcript.read',side_effect=pages)as read:out=list(self.vm.history(self.id));self.assertEqual(read.call_args_list[1].kwargs['cursor'],'opaque')
  self.assertNotIn('cursor',json.dumps(out));self.assertNotIn(q,json.dumps(out));self.assertEqual(before,(a.local(self.id)/(q+'.json')).read_bytes())
 def test_cancel_uses_only_owned_chosen_request_and_hides_transport_ids(self):
  a=self.adapter();q=self.turn(a,'watch_unknown');self.vm.resolve=lambda *_:(a,{})
  value={'phase':'cancel_accepted','terminal':None,'cancel_is_terminal_proof':False,'prompt_retry':False,'next_action':'watch_same_request','request_id':q,'record_id':'hidden','identity':{'private':'metadata'}}
  with patch('request_cancel.cancel',return_value=value)as cancel:result=self.vm.cancel(self.id);cancel.assert_called_once_with(a,self.id,q)
  self.assertEqual(result['phase'],'cancel_accepted');self.assertNotIn(q,json.dumps(result));self.assertNotIn('record_id',result);self.assertFalse(result['cancel_is_terminal_proof'])
 def test_cancel_ambiguous_never_guesses_and_status_is_separate(self):
  a=self.adapter();q=self.turn(a,'queued');self.turn(a,'watch_unknown',revision=2);self.vm.resolve=lambda *_:(a,{})
  with patch('request_cancel.cancel')as cancel:
   self.assertRaisesRegex(RuntimeError,'AMBIGUOUS',self.vm.cancel,self.id);cancel.assert_not_called()
  value={'phase':'cancel_unknown','terminal':None,'cancel_is_terminal_proof':False,'prompt_retry':False,'next_action':'watch_same_request'}
  with patch('request_cancel.status',return_value=value)as status,patch('request_cancel.cancel')as cancel:
   self.assertEqual(self.vm.cancel(self.id,q,status_only=True)['phase'],'cancel_unknown');status.assert_called_once_with(a,self.id,q);cancel.assert_not_called()
 def test_send_unknown_no_replay(self):
  a=self.adapter();from unittest.mock import Mock
  a.submit=Mock(side_effect=RuntimeError('lost'));self.vm.resolve=lambda *_:(a,{})
  one=self.vm.send(self.id,'text');two=self.vm.send(self.id,'text');self.assertEqual(one['state'],'send_unknown');self.assertEqual(two['state'],'send_unknown');self.assertEqual(a.submit.call_count,1)
 def test_pending_prevents_send(self):
  a=self.adapter();self.turn(a,'queued');from unittest.mock import Mock
  a.submit=Mock();self.vm.resolve=lambda *_:(a,{})
  self.assertRaisesRegex(RuntimeError,'UNFINISHED',self.vm.send,self.id,'text');a.submit.assert_not_called();self.assertEqual(self.vm.send(self.id,'text')['state'],'not_dispatched')
 def test_connect_unknown_never_recreates(self):
  a=self.adapter();write(a.local(self.id)/'session.json',{'phase':'create_unknown'});self.vm.resolve=lambda *_:(a,{})
  self.assertEqual(self.vm.connect(self.id)['state'],'create_unknown')
 def test_finish_failed_export_never_stops(self):
  a=self.adapter();self.turn(a);from unittest.mock import Mock
  a.collect=Mock(side_effect=RuntimeError('transfer lost'));a.finish=Mock();self.vm.resolve=lambda *_:(a,{})
  for _ in range(2):self.assertRaisesRegex(RuntimeError,'transfer lost',self.vm.finish,self.id,['result.txt'])
  self.assertEqual(a.collect.call_args_list[0],a.collect.call_args_list[1]);a.finish.assert_not_called()
 def test_finish_never_claims_disk_delete(self):
  a=self.adapter();self.turn(a);from unittest.mock import Mock
  a.collect=Mock();a.finish=Mock(return_value={'phase':'nomad_terminal'});self.vm.resolve=lambda *_:(a,{})
  out=self.vm.finish(self.id,['result.txt']);self.assertTrue(out['results_verified']);self.assertFalse(out['disk_deleted'])
 def test_global_discovery_does_not_mutate_bound(self):
  base={'owner_id':'owner','namespace':'workers'};bound=write(self.root/'bound.json',{**base,'allocation':uid()});global_=write(self.root/'global.json',{**base,'catalog_sources':[{'architecture':'arm64'}]});before=bound.read_bytes()
  self.assertEqual(v.discovery_config(bound,global_),str(global_));self.assertEqual(bound.read_bytes(),before)
 def test_global_discovery_owner_change_rejected(self):
  bound=write(self.root/'bound.json',{'owner_id':'one','namespace':'workers'});global_=write(self.root/'global.json',{'owner_id':'two','namespace':'workers'});self.assertRaisesRegex(RuntimeError,'SCOPE_CHANGED',v.discovery_config,bound,global_)
 def test_creator_symlink_rejected(self):
  p=self.root/'link';p.symlink_to(self.record);self.assertRaises(Exception,v.creator,p,{})
 def test_hook_exports_only_reference(self):
  p=self.root/'native.env';hook.export_context(str(self.record),p);self.assertEqual(p.read_text(),'export NOMAD_WORKERS_CREATOR_RECORD='+str(self.record)+'\n');self.assertNotIn('native-test',p.read_text())
 def test_hook_tool_input_not_read_stale_call_cleared(self):
  folder=self.root/'hooks';hook.record(folder,{'hook_event_name':'SessionStart','session_id':'s'})
  _,row=hook.record(folder,{'hook_event_name':'PreToolUse','session_id':'s','tool_use_id':'call-1','tool_input':{'secret':'do-not-save'}});self.assertNotIn('native_tool_use_id',row);self.assertNotIn('do-not-save',json.dumps(row))
  _,row=hook.record(folder,{'hook_event_name':'PreToolUse','session_id':'s'});self.assertNotIn('native_tool_use_id',row)
 def test_hook_environment_symlink_rejected(self):
  p=self.root/'env';p.symlink_to(self.record);self.assertRaises(OSError,hook.export_context,str(self.record),p)
 def test_parallel_native_calls_have_immutable_distinct_contexts(self):
  from concurrent.futures import ThreadPoolExecutor
  import shlex
  folder=self.root/'parallel';path,row=hook.record(folder,{'hook_event_name':'SessionStart','session_id':'parallel'})
  def invoke(i):
   event={'hook_event_name':'PreToolUse','tool_name':'Bash','session_id':'parallel','tool_use_id':'tool-'+str(i),'tool_input':{'command':'printf original','timeout':123}}
   _,observed=hook.record(folder,event);output=hook.call_context(event,path,observed)['hookSpecificOutput']
   self.assertNotIn('permissionDecision',output);self.assertEqual(output['updatedInput']['timeout'],123)
   prefix,command=output['updatedInput']['command'].split(';\n',1);self.assertEqual(command,'printf original')
   ref=shlex.split(prefix)[1].split('=',1)[1];_,context,_=v.creator(ref,{})
   self.assertEqual(context['native_tool_use_id'],'tool-'+str(i));self.assertNotIn('printf original',pathlib.Path(ref).read_text());return ref
  with ThreadPoolExecutor(max_workers=8)as pool:refs=list(pool.map(invoke,range(24)))
  self.assertEqual(len(set(refs)),24)
 def test_native_call_rejected_after_generation_ends(self):
  data=json.loads(self.record.read_text());current=pathlib.Path(data['native_creator_record']);write(current,{**json.loads(current.read_text()),'ended':True})
  self.assertRaisesRegex(RuntimeError,'CREATOR_CHANGED',v.creator,self.record,{})
 def test_shared_last_call_field_never_used(self):
  data=json.loads(self.record.read_text());data.pop('native_creator_record');write(self.record,data)
  self.assertRaisesRegex(RuntimeError,'PER_CALL',v.creator,self.record,{})
 def test_missing_updated_input_call_context_never_dispatches(self):
  self.vm.native.pop('native_tool_use_id');self.assertRaisesRegex(RuntimeError,'NO_DISPATCH',self.vm.send,self.id,'text')
 def bound(self):
  from types import SimpleNamespace
  self.discovery([{'ID':'bound-allocation','ClientStatus':'running'}]);folder=self.client.root/self.id;folder.mkdir(mode=0o700)
  write(folder/'target-config.json',{'owner_id':self.client.c['owner_id'],'namespace':'workers','agent_instance_id':self.vm.me['agent_id'],'creator_session_id':'native-test','creator_generation':self.vm.me['generation'],'allocation':'bound-allocation'})
  write(folder/'session.json',{'attempt':'same'})
  a=SimpleNamespace(target=lambda:{'Job':self.job(),'ClientStatus':'running'},require_initiator=lambda s:None,attempt=lambda row:'same',load=lambda s:json.loads((folder/'session.json').read_text()))
  self.vm.adapter_factory=lambda *args:a;return a,folder
 def test_bound_target_unchanged_after_resolve(self):
  a,folder=self.bound();before=(folder/'target-config.json').read_bytes();self.assertIs(self.vm.resolve(self.id)[0],a);self.assertEqual((folder/'target-config.json').read_bytes(),before)
 def test_changed_attempt_never_rebound(self):
  a,folder=self.bound();a.attempt=lambda _: 'other';self.assertRaisesRegex(RuntimeError,'ATTEMPT_CHANGED',self.vm.resolve,self.id)
 def test_other_local_creator_rejected(self):
  a,folder=self.bound();p=folder/'target-config.json';data=json.loads(p.read_text());data['creator_generation']=uid();write(p,data);self.assertRaisesRegex(RuntimeError,'LOCAL_OWNER',self.vm.resolve,self.id)
 def test_other_remote_creator_rejected(self):
  a,folder=self.bound();job=self.job();job['Meta']['creator_generation']=uid();a.target=lambda:{'Job':job};self.assertRaisesRegex(RuntimeError,'REMOTE_OWNER',self.vm.resolve,self.id)
 def test_private_lock_permissions_required(self):
  p=self.root/'lock';p.touch();p.chmod(0o644);self.assertRaisesRegex(RuntimeError,'PRIVATE_LOCK',v.private_lock,p)
if __name__=='__main__':unittest.main()
