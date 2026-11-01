import copy
import json
import pathlib
import sys
import tempfile
import unittest
import uuid

sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import catalog_cli
import resource_change
import resume
import test_resume

class ResourceChangeTests(unittest.TestCase):
 def fixture(self):
  config,source,session=test_resume.ResumeTests().fixture()
  config['memory_overhead_mib']=128
  node={'stopped_resource_change':True,'node_id':source['NodeID'],'placement_eligible':True,'cpu_mhz_per_vcpu':1250,'memory_overhead_mib_per_vm':128,'free_cpu_mhz':20000,'free_reservation_mib':16384,'node_operator_limits':{'max_guest_vcpus':8,'max_guest_memory_mib':8192,'max_guest_disk_gib':100,'max_total_disk_gib':200},'occupied':{'driver':{'known':True,'physical_available_mib':100000,'disk_floor_mib':1024,'used_disk_gib':60}}}
  return config,source,session,node
 def test_current_desired_reason_and_nomad_reservation(self):
  config,source,session,node=self.fixture()
  request={'desired':{'vcpus':3,'memory_mib':1536,'disk_gib':30},'reason':'larger approved workspace'}
  change=resource_change.plan(config,source,request,node)
  self.assertEqual(change['current'],{'vcpus':2,'memory_mib':1024,'disk_gib':20})
  self.assertEqual(change['disk_delta_gib'],10)
  self.assertTrue(change['requires_stopped_source']);self.assertFalse(change['automatic_stop']);self.assertFalse(change['online_hotplug'])
  job=resume.plan(config,source,session,change);task=job['TaskGroups'][0]['Tasks'][0]
  self.assertEqual(task['Resources'],{'CPU':3750,'MemoryMB':1664})
  self.assertEqual(task['Config']['guest_cpus'],3);self.assertEqual(task['Config']['memory_mib'],1536);self.assertEqual(task['Config']['disk_gib'],30)
  self.assertEqual(task['Config']['resource_change_reason'],request['reason'])
  self.assertEqual(job['Constraints'][0]['RTarget'],source['NodeID'])
 def test_cpu_ram_reduction_is_distinct_from_disk_shrink(self):
  config,source,session,node=self.fixture()
  change=resource_change.plan(config,source,{'desired':{'vcpus':1,'memory_mib':512},'reason':'smaller working set'},node)
  self.assertEqual(change['nomad_reservation'],{'CPU':1250,'MemoryMB':640})
  self.assertEqual(change['desired']['disk_gib'],20)
  with self.assertRaisesRegex(ValueError,'SHRINK'):resource_change.plan(config,source,{'desired':{'disk_gib':19},'reason':'smaller'},node)
 def test_unsupported_or_unknown_capacity_has_no_submit(self):
  for fault in ('running','no-op','reason','zero','bool','cpu','memory','quota','physical','unknown','other-node','old-driver'):
   with self.subTest(fault=fault):
    config,source,session,node=self.fixture();request={'desired':{'disk_gib':30},'reason':'larger workspace'}
    if fault=='running':source['ClientStatus']='running'
    elif fault=='no-op':request['desired']={'disk_gib':20}
    elif fault=='reason':request['reason']=''
    elif fault=='zero':request['desired']['vcpus']=0
    elif fault=='bool':request['desired']['vcpus']=True
    elif fault=='cpu':node['free_cpu_mhz']=1
    elif fault=='memory':node['free_reservation_mib']=1
    elif fault=='quota':node['node_operator_limits']['max_total_disk_gib']=65
    elif fault=='physical':node['occupied']['driver']['physical_available_mib']=1
    elif fault=='unknown':node['occupied']['driver']['known']=False
    elif fault=='other-node':node['node_id']='other'
    elif fault=='old-driver':node['stopped_resource_change']=False
    with self.assertRaises(ValueError):
     change=resource_change.plan(config,source,request,node);resume.plan(config,source,session,change)
 def test_unknown_submit_same_request_lookup_only_and_target_change_rejected(self):
  config,source,session,node=self.fixture()
  with tempfile.TemporaryDirectory() as tmp:
   config.update(receipt_root=tmp);path=pathlib.Path(tmp)/'config';path.write_text(json.dumps(config))
   creator=pathlib.Path(tmp)/'creator';creator.write_text(json.dumps({'agent_instance_id':config['agent_instance_id'],'native_session_id':'new-native-session','generation':str(uuid.uuid4()),'evidence':'native_hook_observation_not_authenticated_liveness','ended':False}));creator.chmod(0o600)
   client=catalog_cli.CatalogClient(path);client.api=lambda _:source;client.capacity=lambda:{'nodes':[node]};client.request_status=lambda _:{'lookup_only':True};calls=[]
   def run(args,input=None):calls.append(json.loads(input));return type('Result',(),{'returncode':1})()
   client.run=run
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN'):client.change_resources(session,source['ID'],creator,'more disk',disk_gib=30)
   self.assertEqual(client.change_resources(session,source['ID'],creator,'more disk',disk_gib=30),{'lookup_only':True})
   self.assertEqual(len(calls),1)
   with self.assertRaisesRegex(ValueError,'CHANGED'):client.change_resources(session,source['ID'],creator,'more disk',disk_gib=40)
   intent=json.loads((pathlib.Path(tmp)/session/'allocation-intent.json').read_text())
   self.assertEqual(intent['resource_change']['current']['disk_gib'],20);self.assertEqual(intent['resource_change']['desired']['disk_gib'],30)

 def test_caller_limit_and_active_source_refused_before_submit(self):
  for fault in ('caller-limit','active-source'):
   with self.subTest(fault=fault):
    config,source,session,node=self.fixture()
    if fault=='caller-limit':config['request_limits']={'vcpus':2}
    else:source['ClientStatus']='running';source['DesiredStatus']='run'
    with tempfile.TemporaryDirectory() as tmp:
     config['receipt_root']=tmp;path=pathlib.Path(tmp)/'config';path.write_text(json.dumps(config))
     creator=pathlib.Path(tmp)/'creator';creator.write_text(json.dumps({'agent_instance_id':config['agent_instance_id'],'native_session_id':'new-native-session','generation':str(uuid.uuid4()),'evidence':'native_hook_observation_not_authenticated_liveness','ended':False}));creator.chmod(0o600)
     c=catalog_cli.CatalogClient(path);c.api=lambda _:source
     def capacity():
      if fault=='active-source':raise AssertionError('stop boundary should precede capacity planning')
      return {'nodes':[node]}
     c.capacity=capacity
     def run(*args,**kwargs):raise AssertionError('rejected change submitted job')
     c.run=run
     pattern='CALLER_REQUEST_LIMIT' if fault=='caller-limit' else 'STOPPED_TERMINAL'
     with self.assertRaisesRegex(ValueError,pattern):c.change_resources(session,source['ID'],creator,'stage capacity',vcpus=4)
     self.assertFalse((pathlib.Path(tmp)/session/'allocation-intent.json').exists())

if __name__=='__main__':unittest.main()
