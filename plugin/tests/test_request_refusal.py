import copy,json,pathlib,sys,tempfile,unittest,uuid,contextlib,io
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import catalog_cli,workerctl
import test_request_readiness
class RefusalTests(unittest.TestCase):
 def fixture(self,d):
  c,s,root,full,calls=test_request_readiness.RequestReadinessTests().fixture(d)
  full.update(ClientStatus='failed',NodeID='node-fixture',CreateIndex=2);full['TaskStates']['worker'].update(State='dead',StartedAt=None,Events=[{'Type':'Driver Failure','FailsTask':True,'Details':{'driver_error':'node operator quota exceeded: resource=disk_gib requested=30 used=88 total=118 limit=100 (retained/uncertain resources included) API_KEY=synthetic-secret'}}]);full['Job']['TaskGroups']=[{'Tasks':[{'Name':'worker','Config':{'guest_cpus':2,'memory_mib':4096,'disk_gib':30}}]}]
  c.capacity=lambda:{'nodes':[{'node_id':'node-fixture','available_for_one_new_vm':{'disk_gib':12},'occupied':{'driver':{'used_disk_gib':88}},'node_operator_limits':{'max_total_disk_gib':100}}]}
  return c,s,root,full,calls
 def test_quota_failure_terminal_no_submitted_loop_and_safe_details(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d);r=c.request_status(s)
   self.assertEqual(r['phase'],'allocation_failed');self.assertEqual(r['submission_phase'],'submitted');self.assertTrue(r['stop_waiting']);self.assertTrue(r['terminal']);self.assertEqual(r['exit_code'],2);self.assertEqual(r['reason_code'],'NODE_QUOTA_EXCEEDED');self.assertFalse((root/'target-config.json').exists());self.assertNotIn('synthetic-secret',json.dumps(r));self.assertEqual(r['allocations'][0]['events'][0]['quota']['available'],12)
   with patch('catalog_cli.time.sleep',side_effect=AssertionError('must stop waiting')):self.assertEqual(c.request_wait(s,120)['exit_code'],2)
 def test_cli_prints_failure_json_and_exits_nonzero(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d)
   for cmd in ('request-status','request-wait'):
    output=io.StringIO()
    with patch.object(sys,'argv',['workerctl','--config','fixture',cmd,'--session',s]),patch('catalog_cli.CatalogClient',return_value=c),contextlib.redirect_stdout(output),self.assertRaises(SystemExit)as e:workerctl.main()
    self.assertEqual(e.exception.code,2);self.assertEqual(json.loads(output.getvalue())['phase'],'allocation_failed')
 def test_old_failure_does_not_override_newer_complete_history(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,old,calls=self.fixture(d);new=copy.deepcopy(old);new.update(ID=str(uuid.uuid4()),ClientStatus='complete',CreateIndex=3);new['TaskStates']['worker']['Events']=[]
   c.api=lambda path:[{'ID':old['ID'],'ClientStatus':'failed'},{'ID':new['ID'],'ClientStatus':'complete'}]if path.startswith('/v1/job/')else new if new['ID']in path else old
   r=c.request_status(s);self.assertEqual(r['phase'],'allocation_complete');self.assertEqual(r['exit_code'],0);self.assertEqual(r['current_terminal_allocation_id'],new['ID']);self.assertEqual(len(r['allocations']),2)
 def test_unknown_detail_does_not_claim_success_or_missing_vm(self):
  with tempfile.TemporaryDirectory()as d:
   c,s,root,full,calls=self.fixture(d)
   def api(path):
    if path.startswith('/v1/job/'):return [{'ID':full['ID'],'ClientStatus':'failed'}]
    raise RuntimeError('NOMAD_READ_UNAVAILABLE_OR_ACL_DENIED')
   c.api=api;r=c.request_status(s);self.assertEqual(r['exit_code'],2);self.assertTrue(r['allocations'][0]['not_proof_vm_absent']);self.assertTrue(r['allocations'][0]['detail_unavailable'])
if __name__=='__main__':unittest.main()
