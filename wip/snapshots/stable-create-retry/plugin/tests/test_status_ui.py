import pathlib,sys,unittest,types,time,json,subprocess,tempfile,uuid,threading
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import status_ui as ui,catalog_cli
class Status(unittest.TestCase):
 def service(self,n=4,delay=.05):
  owner={'agent_id':str(uuid.uuid4()),'native_session_id':'native','generation':str(uuid.uuid4())};c=types.SimpleNamespace(c={'owner_id':'owner','namespace':'workers'});ids=[str(uuid.uuid4())for _ in range(n)]
  jobs=[{'ID':'worker-'+id,'Status':'running','Meta':{'nomad_workers_owner':'owner','worker_session_id':id,'creator_agent_id':owner['agent_id'],'creator_session_id':'native','creator_generation':owner['generation']}}for id in ids]
  c.api=lambda path:jobs;c.run=lambda *args,**kw:None
  service=types.SimpleNamespace(client=c,me=owner,adapter_factory=lambda *args:None)
  def snapshot(vm):time.sleep(delay);return {'vm_id':vm,'state':'running','task_state':'unknown','task_evidence':'no_exact_anchor'}
  service._status_snapshot=snapshot;return service,ids
 def test_batch_four_snapshots_parallel_not_four_watch_windows(self):
  service,ids=self.service(delay=0);barrier=threading.Barrier(4)
  def snapshot(vm):barrier.wait(timeout=1);return {'vm_id':vm,'state':'running','task_state':'unknown'}
  service._status_snapshot=snapshot;out=ui.listing(service);self.assertEqual(len(out['vms']),4);self.assertTrue(out['snapshot_not_watch']);self.assertTrue(all(v['task_state']=='unknown'for v in out['vms']))
 def test_failure_per_vm_remains_unknown_not_running(self):
  service,ids=self.service(delay=0);original=service._status_snapshot
  def snapshot(vm):
   if vm==ids[0]:raise RuntimeError('ACP_READ_UNAVAILABLE')
   return original(vm)
  service._status_snapshot=snapshot;out=ui.listing(service);self.assertEqual(out['vms'][0]['state'],'unknown');self.assertEqual(out['vms'][0]['error'],'ACP_READ_UNAVAILABLE');self.assertEqual(out['vms'][1]['state'],'running')
 def test_inventory_error_not_empty_success(self):
  service,ids=self.service();service.client.api=lambda path:(_ for _ in ()).throw(RuntimeError('NOMAD_READ_UNAVAILABLE'));out=ui.listing(service);self.assertEqual(out['inventory_state'],'unknown');self.assertIn('inventory_error',out)
 def test_other_native_generation_is_not_mine(self):
  service,ids=self.service();jobs=service.client.api('');jobs[0]['Meta']['creator_generation']='foreign';service.client.api=lambda path:jobs;self.assertEqual(len(ui.listing(service)['vms']),3)
 def test_subprocess_transport_receives_remaining_deadline(self):
  with tempfile.TemporaryDirectory()as d:
   config=pathlib.Path(d)/'config';config.write_text(json.dumps({'receipt_root':d,'nomad_bin':'/never/execute'}));client=catalog_cli.CatalogClient(config);service=types.SimpleNamespace(client=client,adapter_factory=lambda *args:None)
   def snapshot(vm):service.client.api('/v1/nodes')
   # Bind a real method to the cloned service so it uses the bounded client.
   class Real:
    def _status_snapshot(self,vm):return {'observed':self.client.api('/v1/nodes')}
   service=Real();service.client=client;service.adapter_factory=lambda *args:None
   with patch('catalog_cli.subprocess.run',side_effect=subprocess.TimeoutExpired('synthetic',.1))as run:out=ui.read(service,'id',seconds=.1)
   self.assertEqual(out['state'],'unknown');self.assertGreater(run.call_args.kwargs['timeout'],0);self.assertLessEqual(run.call_args.kwargs['timeout'],.1)
 def test_expired_budget_does_not_start_transport(self):
  service,ids=self.service(delay=0);clone=ui.bounded(service,time.monotonic()-1);self.assertRaisesRegex(RuntimeError,'DEADLINE',clone.client.run,['operator','api'])
 def test_wait_is_distinct_and_stops_on_exact_terminal_without_cancel(self):
  service,ids=self.service(delay=0)
  with patch('status_ui.read',return_value={'state':'running','task_state':'terminal','work_items':[]}):out=list(ui.wait(service,ids[0],1))
  self.assertEqual(out[0]['wait_state'],'terminal_observed')
 def test_wait_deadline_keeps_unknown_and_does_not_cancel(self):
  service,ids=self.service(delay=0)
  with patch('status_ui.read',return_value={'state':'running','task_state':'unknown','work_items':[]}),patch('status_ui.time.monotonic',side_effect=[0,0,2]):out=list(ui.wait(service,ids[0],1))
  self.assertEqual(out[-1]['wait_state'],'deadline_reached');self.assertTrue(out[-1]['vm_retained']);self.assertFalse(out[-1]['cancellation_sent'])
if __name__=='__main__':unittest.main()
