import json,pathlib,sys,tempfile,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
from catalog_cli import CatalogClient
class ClusterCapacityTests(unittest.TestCase):
 def fixture(self,root):
  p=pathlib.Path(root)/'config';p.write_text(json.dumps({'receipt_root':root,'memory_overhead_mib':128}));c=CatalogClient(p)
  listings=[{'ID':'linux','Status':'ready','SchedulingEligibility':'eligible'},{'ID':'mac','Status':'ready','SchedulingEligibility':'eligible'},{'ID':'down','Status':'disconnected','SchedulingEligibility':'eligible'}]
  def api(path):
   if path.startswith('/v1/nodes'):return listings
   name=path.split('/')[3]
   if path.endswith('/allocations'):
    return [{'ID':'busy','ClientStatus':'running','AllocatedResources':{'Tasks':{'w':{'Cpu':{'CpuShares':4000},'Memory':{'MemoryMB':2048}}}}}] if name=='linux' else []
   return {'ID':name,'Name':name,'Attributes':{'cpu.arch':'amd64' if name=='linux' else 'arm64','kernel.name':'linux' if name=='linux' else 'darwin'},'Drivers':{'lima':{'Detected':True,'Healthy':True,'Attributes':{'driver.lima.cpu.nomad_compute_per_core':'2000'}}} if name!='mac' else {'boxlite':{'Detected':True,'Healthy':True}},'NodeResources':{'Cpu':{'CpuShares':4000},'Memory':{'MemoryMB':8192}}}
  c.api=api;return c
 def test_all_nodes_visible_but_only_ready_lima_is_placement_candidate(self):
  with tempfile.TemporaryDirectory() as d:
   rows={x['node_id']:x for x in self.fixture(d).capacity()['nodes']};self.assertEqual(set(rows),{'linux','mac','down'})
   self.assertTrue(rows['linux']['placement_eligible']);self.assertEqual(rows['linux']['available_for_one_new_vm']['vcpus'],0);self.assertEqual(rows['linux']['occupied']['reservation_mib'],2048)
   self.assertEqual(rows['mac']['architecture'],'arm64');self.assertFalse(rows['mac']['placement_eligible']);self.assertIn('lima_not_detected',rows['mac']['placement_reasons']);self.assertIsNone(rows['mac']['available_for_one_new_vm']['guest_memory_mib']);self.assertIsNone(rows['mac']['free_guest_vcpus_by_cpu'])
   self.assertFalse(rows['down']['placement_eligible']);self.assertIn('node_not_ready',rows['down']['placement_reasons'])
 def test_denied_details_remain_visible_unknown_not_eligible(self):
  with tempfile.TemporaryDirectory() as d:
   c=self.fixture(d);original=c.api
   def api(path):
    if path=='/v1/node/mac':raise RuntimeError('NOMAD_READ_UNAVAILABLE_OR_ACL_DENIED')
    return original(path)
   c.api=api;rows={x['node_id']:x for x in c.capacity()['nodes']};self.assertIn('mac',rows);self.assertFalse(rows['mac']['placement_eligible']);self.assertIn('node_detail_unavailable',rows['mac']['placement_reasons']);self.assertIsNone(rows['mac']['available_for_one_new_vm'])
