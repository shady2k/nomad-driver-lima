import json,os,pathlib,sys,tempfile,unittest,uuid
from types import SimpleNamespace
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,template_save as save
import test_checkpoint_activity as checkpoints
import test_template_stage_bindings as bindings
source=bindings.source;node=bindings.node

class CloneLifecycleTests(unittest.TestCase):
 def fixture(self,d):
  root=pathlib.Path(d);caller=root/'caller';caller.mkdir(mode=0o700);a,s,r,allocation,guest=checkpoints.CheckpointTests().fixture(str(caller));session=a.load(s);a.c['vault']={'enabled':True};a.hash=w.digest(w.canonical(a.c));session['config_hash']=a.hash;w.atomic(a.local(s)/'session.json',session)
  snap=str(uuid.uuid4());manifest=a.collect(s,r,['result.txt'],snap);packet=bindings.BindingTests().packet();src=packet['plan']['source'];src.update(session_id=s,allocation=a.c['allocation'],job=a.c['job'],task=a.c['task'],config_sha256=a.hash,attempt=a.attempt(allocation),creator_agent_id=a.c['agent_instance_id'],creator_session_id=a.c['creator_session_id'],creator_generation=a.c['creator_generation']);packet['plan']['snapshot']={'request_id':r,'snapshot_id':snap,'manifest_sha256':save.sha(manifest)};packet['plan_sha256']=save.sha(packet['plan'])
  state_dir=root/'ledger';state_dir.mkdir(mode=0o700);home=root/'home';home.mkdir(mode=0o700);instance=home/'source';instance.mkdir();(instance/'diffdisk').write_bytes(b'prepared-disk');key='a'*64
  state={'Key':key,'Owner':'owner','AllocID':src['allocation'],'TaskName':src['task'],'WorkerSessionID':s,'CreatorAgentID':src['creator_agent_id'],'CreatorSessionID':src['creator_session_id'],'Phase':'running','MachineID':'machine','BootID':'boot','Instance':'source'};path=state_dir/(key+'.json');w.atomic(path,state);(state_dir/'owner').write_text('owner');original=a.nomad
  def nomad(args,**kw):
   result=original(args,**kw)
   if args[:2]==['job','stop']:
    allocation['TaskStates']['worker']['Events']=[{'DriverMessage':'lima task_token_revoked'},{'DriverMessage':'lima vm_stopped'}];state.update(Phase='stopped',VaultFinalizing=True);w.atomic(path,state)
   return result
  a.nomad=nomad;receipt=source.finish(a,packet);calls=[]
  def run(args,**kw):
   calls.append(args)
   if args[1]=='list':name=args[-1];return SimpleNamespace(returncode=0,stdout=json.dumps({'name':name,'status':'Stopped','dir':str(home/name)}))
   self.assertEqual(args[1],'clone');dst=home/args[-1];dst.mkdir();(dst/'diffdisk').write_bytes(b'prepared-disk');return SimpleNamespace(returncode=0,stdout='')
  cfg={'ledger_key':key,'state_dir':str(state_dir),'lima_home':str(home)}
  return packet,{'source_finish':receipt},cfg,path,home,calls,run
 def test_real_finish_snapshot_then_clone_accepts_terminal_durable_fence(self):
  with tempfile.TemporaryDirectory()as d:
   p,receipts,cfg,path,home,calls,run=self.fixture(d);before=path.read_bytes()
   result=node.bound_clone(p,receipts,cfg,lambda p,r,c:node.clone.clone(p,r,home,'/official/limactl',run))
   self.assertTrue(result['clone_stopped']);self.assertEqual(path.read_bytes(),before);self.assertTrue(json.loads(before)['VaultFinalizing']);self.assertEqual(sum(x[1]=='clone'for x in calls),1)
 def test_pre_effect_proof_no_clone_then_changed_hash_rejected(self):
  with tempfile.TemporaryDirectory()as d:
   p,receipts,cfg,path,home,calls,run=self.fixture(d)
   proof=node.bound_clone(p,receipts,cfg,lambda p,r,c:node.clone.clone(p,r,home,'/official/limactl',run,check_only=True));self.assertTrue(proof['candidate_absent']);self.assertFalse(any(x[1]=='clone'for x in calls))
   (home/'source/diffdisk').write_bytes(b'changed-after-proof');receipts['clone_pre_effect_proof']=proof
   with self.assertRaisesRegex(RuntimeError,'CHANGED_AFTER_CLONE_PRE_EFFECT'):node.bound_clone(p,receipts,cfg,lambda *a:self.fail('no clone'))
 def test_unfinished_cleanup_and_missing_revoke_proof_rejected(self):
  for mode in ('unfinished','unrevoked'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory()as d:
    p,receipts,cfg,path,home,calls,run=self.fixture(d)
    if mode=='unfinished':state=json.loads(path.read_text());state['Phase']='exited';w.atomic(path,state)
    else:receipts['source_finish']['task_token_revoked']=False
    with self.assertRaises(RuntimeError):node.bound_clone(p,receipts,cfg,lambda *a:self.fail('must not clone'))
 def test_partial_candidate_and_live_pid_block_pre_effect_retry(self):
  for mode in ('partial','pid'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory()as d:
    p,receipts,cfg,path,home,calls,run=self.fixture(d)
    if mode=='partial':(home/('agent-base-build-save-'+p['plan']['operation_id'].replace('-',''))).mkdir()
    else:(home/'source/qemu.pid').write_text(str(os.getpid()))
    with self.assertRaisesRegex(RuntimeError,'CANDIDATE_EXISTS|PROCESS_STILL'):node.bound_clone(p,receipts,cfg,lambda p,r,c:node.clone.clone(p,r,home,'/official/limactl',run,check_only=True))
    self.assertFalse(any(x[1]=='clone'for x in calls))

 def test_retained_preflight_runs_node_publisher_without_source_finish_replay(self):
  from unittest import mock
  import pwd
  module=bindings.load('retained_preflight','template-preflight.py');node_preflight=bindings.load('retained_node','template-preflight-node.py')
  with tempfile.TemporaryDirectory()as d:
   p,receipts,cfg,path,home,calls,run=self.fixture(d);root=pathlib.Path(d);cfg.update(runtime_user=pwd.getpwuid(os.getuid()).pw_name,build_root=str(root/'builds'),disk_gib=20);before=path.read_bytes()
   with mock.patch.object(node_preflight.node,'invoke',side_effect=lambda p,r,c,check_only=False:node.clone.clone(p,r,home,'/official/limactl',run,check_only=check_only)),mock.patch.object(node_preflight.os,'statvfs',return_value=SimpleNamespace(f_bavail=1000000000,f_frsize=4096)),mock.patch.object(node_preflight.sys,'platform','darwin'):
    node_proof=node_preflight.preflight(p,cfg,receipts)
   seen=[]
   def command(args,**kw):
    seen.append((args[0],json.loads(kw['input'])['stage']));values=node_proof if args[0]=='/node'else {'operation_id':p['plan']['operation_id'],'plan_sha256':p['plan_sha256'],'scoped_s3_writer_available':True,'catalog_cas_verified':True,'artifact_transport_binding_ready':True}
    return SimpleNamespace(returncode=0,stdout=json.dumps(values))
   result=module.preflight(p,{'source_preflight':['/must-not-replay'],'node_preflight':['/node'],'publisher_preflight':['/publisher']},command,receipts)
   self.assertTrue(result['source_owner_current']);self.assertEqual(seen,[('/node','preflight_retained_source'),('/publisher','preflight')]);self.assertEqual(path.read_bytes(),before);self.assertFalse(any(x[1]=='clone'for x in calls))

 def test_deployed_ledger_without_generation_accepts_plan_bound_legacy_finish(self):
  with tempfile.TemporaryDirectory()as d:
   p,receipts,cfg,path,home,calls,run=self.fixture(d);self.assertNotIn('CreatorGeneration',json.loads(path.read_text()));self.assertIn('source_creator_identity',receipts['source_finish']);receipts['source_finish'].pop('source_creator_identity')
   result=node.bound_clone(p,receipts,cfg,lambda p,r,c:node.clone.clone(p,r,home,'/official/limactl',run,check_only=True));self.assertTrue(result['clone_effects_absent'])
 def test_wrong_generation_or_changed_plan_cannot_borrow_legacy_finish(self):
  for mode in ('ledger','receipt','plan'):
   with self.subTest(mode=mode),tempfile.TemporaryDirectory()as d:
    p,receipts,cfg,path,home,calls,run=self.fixture(d)
    if mode=='ledger':state=json.loads(path.read_text());state['CreatorGeneration']='other';w.atomic(path,state)
    elif mode=='receipt':receipts['source_finish']['source_creator_identity']['creator_generation']='other'
    else:p['plan']['source']['creator_generation']='other';p['plan_sha256']=save.sha(p['plan'])
    with self.assertRaisesRegex(RuntimeError,'GENERATION_CHANGED|STALE_OR_UNBOUND'):node.bound_clone(p,receipts,cfg,lambda *a:self.fail('must not invoke'))

 def test_actual_lima_pid_fields_reject_active_or_unknown_metadata(self):
  for field,value in (('driverPID',1),('hostAgentPID',1),('driverPID','unknown')):
   with self.subTest(field=field,value=value),tempfile.TemporaryDirectory()as d:
    p,receipts,cfg,path,home,calls,run=self.fixture(d);original=run
    def metadata(args,**kw):
     result=original(args,**kw)
     if args[1]=='list':row=json.loads(result.stdout);row.update(driverPID=0,hostAgentPID=0);row[field]=value;result.stdout=json.dumps(row)
     return result
    with self.assertRaisesRegex(RuntimeError,'PROCESS_STILL_PRESENT_OR_UNKNOWN'):node.bound_clone(p,receipts,cfg,lambda p,r,c:node.clone.clone(p,r,home,'/official/limactl',metadata,check_only=True))
    self.assertFalse(any(x[1]=='clone'for x in calls))
