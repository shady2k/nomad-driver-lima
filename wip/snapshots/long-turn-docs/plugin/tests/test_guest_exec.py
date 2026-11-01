import sys,pathlib,tempfile,json,uuid,unittest,subprocess
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import workerctl as w,guest_exec,catalog_cli
class ExecTests(unittest.TestCase):
 def fixture(self,d):
  s=str(uuid.uuid4());config={'version':1,'allocation':str(uuid.uuid4()),'owner_id':str(uuid.uuid4()),'receipt_root':d,'namespace':'agent-workers','job':'worker-'+s,'task':'worker','agent_instance_id':str(uuid.uuid4()),'creator_session_id':'native-test','creator_generation':str(uuid.uuid4()),'creator_record':str(pathlib.Path(d)/'owner.json'),'guest':{'workspace':'/workspace/repo'}}
  owner={'ended':False,'agent_instance_id':config['agent_instance_id'],'native_session_id':config['creator_session_id'],'generation':config['creator_generation']};w.atomic(config['creator_record'],owner)
  cp=pathlib.Path(d)/'config';cp.write_text(json.dumps(config));a=w.Adapter(cp)
  policy=pathlib.Path(d)/'operation-policy.json';policy.write_text((pathlib.Path(__file__).parents[1]/'config.operations.example.json').read_text());policy.chmod(0o600);a.operations_policy_file=str(policy)
  row={'ID':config['allocation'],'Namespace':config['namespace'],'ClientStatus':'running','Job':{'ID':config['job'],'Version':0,'Meta':{'nomad_workers_owner':config['owner_id'],'creator_agent_id':config['agent_instance_id'],'creator_session_id':config['creator_session_id'],'creator_generation':config['creator_generation'],'worker_session_id':s},'TaskGroups':[{'Tasks':[{'Name':'worker','Driver':'lima'}]}]},'TaskStates':{'worker':{'State':'running','StartedAt':'today','Restarts':0}}}
  a.target=lambda:row;a.event=lambda *args:None;w.atomic(a.local(s)/'session.json',{'config_hash':a.hash,'phase':'ready','attempt':a.attempt(row)})
  return a,s,row,owner
 def test_exact_native_transport_failed_exit_redaction_and_replay(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,row,_=self.fixture(d);eid=str(uuid.uuid4());frame={'version':1,'outcome':'completed','exit_code':7,'stdout':'API_KEY=private\nhello','stderr':'Authorization: Bearer private','stdout_truncated':False,'stderr_truncated':False}
   with patch.object(a,'nomad',return_value=subprocess.CompletedProcess([],0,json.dumps(frame),'')) as n:
    r=guest_exec.legacy_execute(a,s,eid,['/bin/false']);self.assertEqual(r['exit_code'],7);self.assertNotIn('private',json.dumps(r));self.assertEqual(guest_exec.legacy_execute(a,s,eid,['/bin/false']),r);self.assertEqual(n.call_count,1)
    args=n.call_args.args[0];self.assertEqual(args[:2],['alloc','exec']);self.assertIn(a.c['allocation'],args);self.assertEqual(args[-3:-1],['/usr/bin/python3','-c'])
    with self.assertRaisesRegex(RuntimeError,'CONTENT_CHANGED'):guest_exec.legacy_execute(a,s,eid,['/bin/true'])
 def test_wrong_session_generation_and_attempt_never_execute(self):
  for field in ['worker_session_id','creator_session_id','creator_generation']:
   with tempfile.TemporaryDirectory() as d:
    a,s,row,_=self.fixture(d);row['Job']['Meta'][field]='wrong'
    with patch.object(a,'nomad') as n:
     with self.assertRaisesRegex(RuntimeError,'OWNER_MISMATCH'):guest_exec.legacy_execute(a,s,str(uuid.uuid4()),['id'])
     n.assert_not_called()
  with tempfile.TemporaryDirectory() as d:
   a,s,row,o=self.fixture(d);o['generation']=str(uuid.uuid4());w.atomic(a.c['creator_record'],o)
   with self.assertRaisesRegex(RuntimeError,'GENERATION_CHANGED'):guest_exec.legacy_execute(a,s,str(uuid.uuid4()),['id'])
 def test_unknown_no_replay_and_logs(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,_,_=self.fixture(d);eid=str(uuid.uuid4());events=[];a.event=lambda s,e:events.append(e)
   with patch.object(a,'nomad',side_effect=subprocess.TimeoutExpired('fixture',1)) as n:
    with self.assertRaisesRegex(RuntimeError,'UNKNOWN_NO_RETRY'):guest_exec.legacy_execute(a,s,eid,['id'])
    self.assertEqual(guest_exec.legacy_execute(a,s,eid,['id'])['phase'],'exec_unknown');self.assertEqual(n.call_count,1)
   self.assertEqual(events,['guest_exec_started','guest_exec_unknown'])
 def test_export_snapshot_cannot_be_invalidated_by_new_exec(self):
  with tempfile.TemporaryDirectory() as d:
   a,s,_,_=self.fixture(d);w.atomic(a.local(s)/(str(uuid.uuid4())+'.json'),{'export_staging_dir':'frozen-snapshot','export_verified':False})
   with patch.object(a,'nomad') as n:
    with self.assertRaisesRegex(RuntimeError,'UNVERIFIED_EXPORT'):guest_exec.legacy_execute(a,s,str(uuid.uuid4()),['id'])
    n.assert_not_called()
 def test_guest_runner_exit_and_stdout_stderr(self):
  with tempfile.TemporaryDirectory() as d:
   p=subprocess.run([sys.executable,'-c',guest_exec.GUEST_RUNNER],input=json.dumps({'argv':[sys.executable,'-c','import sys;print("out");print("err",file=sys.stderr);sys.exit(7)'],'cwd':d,'timeout':5}),capture_output=True,text=True)
   r=json.loads(p.stdout);self.assertEqual(r['exit_code'],7);self.assertEqual(r['stdout'],'out\n');self.assertEqual(r['stderr'],'err\n')
 def test_capacity_floor_and_overhead(self):
  with tempfile.TemporaryDirectory() as d:
   p=pathlib.Path(d)/'c';p.write_text(json.dumps({'receipt_root':d,'memory_overhead_mib':128}));c=catalog_cli.CatalogClient(p)
   node={'ID':'node','Name':'fixture','Drivers':{'lima':{'Detected':True,'Healthy':True,'Attributes':{'driver.lima.cpu.nomad_compute_per_core':'2000','driver.lima.operator_limits.max_vms':'0','driver.lima.operator_limits.max_guest_vcpus':'3'}}},'Attributes':{'cpu.arch':'amd64'},'NodeResources':{'Cpu':{'CpuShares':7999},'Memory':{'MemoryMB':4096}}}
   c.api=lambda path:([{'ID':'node','Status':'ready','SchedulingEligibility':'eligible'}] if path.startswith('/v1/nodes') else [] if path.endswith('/allocations') else node)
   r=c.capacity()['nodes'][0];self.assertEqual(r['free_guest_vcpus_by_cpu'],3);self.assertEqual(r['free_guest_memory_mib_for_one_vm'],3968);self.assertEqual(r['available_for_one_new_vm']['configured_caps']['max_vms'],'no_additional_cap');self.assertEqual(r['available_for_one_new_vm']['vcpus'],3)
if __name__=='__main__':unittest.main()
