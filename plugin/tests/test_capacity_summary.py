import copy,json,pathlib,sys,tempfile,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
from unittest.mock import patch
import capacity_view as cv,catalog_routing
import test_cluster_capacity as cluster
class Summary(unittest.TestCase):
 def result(self):
  def node(id,arch,cpu,mem,disk):return {'node_id':id,'architecture':arch,'host_os':'darwin'if arch=='arm64'else'linux','node_status':'ready','placement_eligible':True,'placement_reasons':[],'observed_unix':123,'available_for_one_new_vm':{'vcpus':cpu,'guest_memory_mib':mem,'disk_gib':disk}}
  return {'nodes':[node('linux','amd64',0,2789,52),node('mac','arm64',2,20224,64)],'resource_scope':'per_node_not_interchangeable_or_summed'}
 def test_full_first_node_does_not_hide_free_second_node(self):
  result=self.result();before=copy.deepcopy(result);out=cv.summarize(result);rows=out['capacity_summary'];self.assertEqual([r['node_id']for r in rows],['linux','mac']);self.assertEqual(rows[0]['vcpus'],0);self.assertEqual(rows[1]['vcpus'],2);self.assertEqual(rows[1]['guest_memory_mib'],20224);self.assertEqual(out['nodes'],before['nodes']);self.assertEqual(result,before);self.assertEqual(next(iter(out)),'capacity_summary');self.assertIn('never authorizes finish',out['capacity_guidance'])
 def test_maintenance_unknown_is_null_not_zero_or_hardware_capacity(self):
  result=self.result();result['nodes'][1].update(placement_eligible=False,placement_reasons=['node_not_eligible']);row=cv.summarize(result)['capacity_summary'][1];self.assertFalse(row['eligible']);self.assertIsNone(row['vcpus']);self.assertIsNone(row['guest_memory_mib']);self.assertIsNone(row['disk_gib']);self.assertEqual(row['reasons'],['node_not_eligible'])
 def test_unknown_details_keep_row_visible(self):
  result={'nodes':[{'node_id':'denied','available_for_one_new_vm':None,'placement_reasons':['node_detail_unavailable']}]};row=cv.summarize(result)['capacity_summary'][0];self.assertEqual(row['node_id'],'denied');self.assertIsNone(row['vcpus']);self.assertIsNone(row['observed_unix'])
 def test_real_capacity_path_includes_every_node_and_legacy_details(self):
  with tempfile.TemporaryDirectory()as d:
   c=cluster.ClusterCapacityTests().fixture(d);out=c.capacity();self.assertEqual({r['node_id']for r in out['capacity_summary']},{n['node_id']for n in out['nodes']});self.assertEqual(len(out['nodes']),3);self.assertEqual(out['capacity_summary'][0]['vcpus'],0)
 def test_catalog_rejection_updates_aliases_before_summary(self):
  with tempfile.TemporaryDirectory()as d:
   c=cluster.ClusterCapacityTests().fixture(d);c.templates=lambda:[]
   with patch('catalog_routing.enabled',return_value=True):out=c.capacity()
   self.assertTrue(all(r['vcpus']is None for r in out['capacity_summary']));self.assertTrue(all(n['effective_available']['vcpus']is None for n in out['nodes']));self.assertTrue(all('catalog_source_unrecognized'in r['reasons']for r in out['capacity_summary']))
if __name__=='__main__':unittest.main()
