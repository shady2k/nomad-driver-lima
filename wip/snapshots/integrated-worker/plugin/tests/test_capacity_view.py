import sys,pathlib,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
from capacity_view import available
class CapacityTests(unittest.TestCase):
 def test_occupied_caps_physical_and_overhead(self):
  limits={'max_vms':8,'max_total_vcpus':8,'max_total_memory_mib':30000,'max_total_disk_gib':100,'max_guest_disk_gib':40}
  t={'known':True,'observed_unix':100,'used_vms':1,'used_vcpus':2,'used_reservation_mib':4096,'used_disk_gib':80,'physical_available_mib':25000,'disk_floor_mib':4096}
  r=available(15999,28000,2000,128,limits,t,now=110);self.assertEqual(r['vcpus'],6);self.assertEqual(r['guest_memory_mib'],25776);self.assertEqual(r['disk_gib'],20);self.assertEqual(r['additional_vm_slots'],7)
  limits['max_total_vcpus']=0;r=available(15999,28000,2000,128,limits,t,now=110);self.assertEqual(r['vcpus'],7);self.assertEqual(r['configured_caps']['max_total_vcpus'],'no_additional_cap')
  t['physical_available_mib']=5000;r=available(15999,28000,2000,128,limits,t,now=110);self.assertEqual(r['disk_gib'],0)
 def test_unknown_stale_no_disk_promise(self):
  for t in ({},{'known':False},{'known':True,'observed_unix':0}):
   r=available(15999,28000,2000,128,{},t,now=110);self.assertEqual(r['vcpus'],7);self.assertIsNone(r['disk_gib']);self.assertEqual(r['telemetry'],'unknown')
if __name__=='__main__':unittest.main()
