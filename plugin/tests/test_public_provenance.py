import sys,pathlib,tempfile,json,uuid,unittest,subprocess
from unittest.mock import patch
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import provenance,session_hook,workerctl as w
import test_catalog_lifecycle as fixtures
class ProvenanceTests(unittest.TestCase):
 def test_name_type_separate_and_missing_fields_explicit(self):
  with self.assertRaisesRegex(ValueError,'TASK_TITLE'):provenance.descriptions({}, {},None)
  with self.assertRaisesRegex(ValueError,'NAME_TYPE'):provenance.descriptions({}, {},'Compile project changes')
  v=provenance.descriptions({'source_agent':{'name':'Review coordinator'}},{'native_agent_type':'Claude','native_agent_id':'native-7'},'Compile project changes')
  self.assertEqual(v['source_agent_name'],'Review coordinator');self.assertEqual(v['source_agent_type'],'Claude');self.assertEqual(v['source_agent_name_evidence'],'operator_config');self.assertEqual(v['source_task_link_status'],'not_provided')
  with self.assertRaisesRegex(ValueError,'CREDENTIAL_FREE|INVALID_DESCRIPTIVE_TASK_LINK'):provenance.descriptions({'source_agent':{'name':'Review coordinator','type':'Claude'}},{},'Compile project changes',link='https://issues.example.invalid/task?token=hidden')
 def test_hook_optional_descriptions_do_not_change_owner_on_tool_event(self):
  with tempfile.TemporaryDirectory()as d:
   path,owner=session_hook.record(d,{'hook_event_name':'SessionStart','session_id':'native-test','agent_name':'Review coordinator','agent_type':'Claude','agent_id':'native-7','source':'startup'},now=1)
   _,again=session_hook.record(d,{'hook_event_name':'PreToolUse','session_id':'native-test','agent_name':'forged'},now=2)
   self.assertEqual(again['native_agent_name'],'Review coordinator');self.assertEqual(again['generation'],owner['generation']);self.assertEqual(again['agent_instance_id'],owner['agent_instance_id'])
 def test_request_metadata_is_distinct_from_runtime_model_and_lookup_stable(self):
  with tempfile.TemporaryDirectory()as d:
   c,s=fixtures.CatalogTests().client(d);c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000}]};jobs=[]
   c.run=lambda args,input=None:(jobs.append(json.loads(input))or type('R',(),{'returncode':0})());c.request_status=lambda sid:{'lookup_only':True}
   args=(s,'agent-base','v1','amd64',1,1024,20,'build')
   kw={'task_title':'Compile project changes','source_agent_name':'Review coordinator','source_agent_type':'Claude'}
   c.request(*args,**kw);meta=jobs[0]['Job']['Meta'];self.assertEqual(meta['source_task_title'],kw['task_title']);self.assertEqual(meta['source_task_link_status'],'not_provided');self.assertEqual(meta['task_link'],'');self.assertEqual(meta['source_native_session_id'],'');self.assertEqual(meta['source_native_session_status'],'not_observed');self.assertNotIn('runtime_coordinator_model',meta)
   c.request(*args,**kw);self.assertEqual(len(jobs),1)
   with self.assertRaisesRegex(ValueError,'CHANGED'):c.request(*args,**{**kw,'task_title':'Different project task'})
 def test_hook_request_to_nomad_preserves_native_origin(self):
  with tempfile.TemporaryDirectory()as d:
   c,s=fixtures.CatalogTests().client(d);path,native=session_hook.record(pathlib.Path(d)/'initiators',{'hook_event_name':'SessionStart','session_id':'actual-native-session','agent_name':'Review coordinator','agent_type':'Claude','agent_id':'native-agent-7','source':'resume'})
   c.capacity=lambda:{'nodes':[{'architecture':'amd64','host_os':'linux','cpu_mhz_per_vcpu':1000}]};jobs=[];c.run=lambda args,input=None:(jobs.append(json.loads(input))or type('R',(),{'returncode':0})());c.request_status=lambda sid:{}
   c.request(s,'agent-base','v1','amd64',1,1024,20,'build',creator_record=path,task_title='Compile project changes')
   meta=jobs[0]['Job']['Meta'];self.assertEqual(meta['creator_agent_id'],native['agent_instance_id']);self.assertEqual(meta['creator_generation'],native['generation']);self.assertEqual(meta['source_native_session_id'],'actual-native-session');self.assertEqual(meta['source_agent_name'],'Review coordinator');self.assertEqual(meta['source_native_agent_id'],'native-agent-7')
 def test_legacy_intent_lookup_does_not_require_new_descriptive_fields(self):
  with tempfile.TemporaryDirectory()as d:
   c,s=fixtures.CatalogTests().client(d);c.request_status=lambda sid:{'lookup_only':True}
   # Actual historical hash contract: no new provenance fields before lookup.
   import hashlib
   h=hashlib.sha256(w.canonical({'config':c.c,'session':s,'template':'agent-base','version':'v1','architecture':'amd64','vcpus':1,'memory_mib':1024,'disk_gib':20,'purpose':'build','project':'','link':'','creator_agent':c.c['agent_instance_id'],'creator_session':s,'creator_generation':None,'coordinator_model':None})).hexdigest()
   root=c.root/s;root.mkdir();w.atomic(root/'allocation-intent.json',{'input_hash':h})
   self.assertTrue(c.request(s,'agent-base','v1','amd64',1,1024,20,'build')['lookup_only'])
if __name__=='__main__':unittest.main()
