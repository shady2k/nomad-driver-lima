import copy,hashlib,importlib.util,json,pathlib,sys,tempfile,unittest,uuid
from unittest import mock
ROOT=pathlib.Path(__file__).parents[1];sys.path.insert(0,str(ROOT/'scripts'))
import model_catalog,workerctl
spec=importlib.util.spec_from_file_location('models_guest',ROOT/'guest/task-main.py');guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)
class AllModelTests(unittest.TestCase):
 def config(self,d):
  raw=(ROOT/'tests/fixtures/provider-models.f02d.json').read_bytes();p=pathlib.Path(d)/'models.json';p.write_bytes(raw)
  return {'model_catalog_file':str(p),'model_catalog_sha256':hashlib.sha256(raw).hexdigest(),'guest':{'model':None,'runtime_contract':{'approved_total_usd':1,'output_token_budget':512},'prime_vault':{'provider':{'name':'fixture-gateway','base_url':'https://gateway.invalid/v1','model':'deepseek-v4.1-flash-auto','max_tokens':16384,'context_window':1000000}}}}
 def test_all_ten_one_gateway_two_native_groups_no_compat_contamination(self):
  with tempfile.TemporaryDirectory() as d:
   c=self.config(d);before=copy.deepcopy(c);g=model_catalog.select_guest(c);p=g['prime_vault']['provider'];session=str(uuid.uuid4());native=model_catalog.native_json(p,session);runtime,selected=guest.native_gateway(p,session)
   self.assertEqual(c,before);self.assertEqual(native,runtime);self.assertEqual(len(native['providers']),2);models={m['id']:block for block in native['providers'].values() for m in block['models']};self.assertEqual(len(models),10);self.assertIn('gpt-6.1-sol',models)
   for b in native['providers'].values():self.assertEqual(b['baseUrl'],'https://gateway.invalid/v1');self.assertEqual(b['apiKey'],'LLM_API_KEY');self.assertEqual(b['headers'],{'x-opencode-session':session})
   self.assertTrue(models['deepseek-v4.1-flash-auto']['compat']['requiresReasoningContentOnAssistantMessages']);self.assertNotIn('requiresReasoningContentOnAssistantMessages',models['gpt-6.1-sol']['compat']);self.assertEqual(selected['model'],'deepseek-v4.1-flash-auto')
   self.assertNotIn('approved_total_usd',g['runtime_contract']);self.assertNotIn('output_token_budget',g['runtime_contract']);self.assertFalse(g['runtime_contract']['plugin_monetary_approval_required'])
 def test_coordinator_and_workers_independent_exact_native_values(self):
  with tempfile.TemporaryDirectory() as d:
   c=self.config(d);g=model_catalog.select_guest(c,'gpt-6.1-sol');p=g['prime_vault']['provider'];mapping=model_catalog.selectors(p);pair=json.loads(mapping['gpt-6.1-sol']['coordinator_acp_value']);self.assertEqual(pair[1],'gpt-6.1-sol');self.assertEqual(p['context_window'],272000);self.assertEqual(p['max_tokens'],128000)
   self.assertEqual(model_catalog.worker_selector(p,'deepseek-v4.1-flash-auto'),mapping['deepseek-v4.1-flash-auto']['worker_selector']);self.assertNotEqual(pair[0],mapping['deepseek-v4.1-flash-auto']['provider']);self.assertIsNone(model_catalog.worker_selector(p));self.assertEqual(p['model'],'gpt-6.1-sol')
   with self.assertRaises(ValueError):model_catalog.select_guest(c,'sol')
   with self.assertRaises(ValueError):model_catalog.worker_selector(p,'sol')
   self.assertEqual(len(model_catalog.listing(c)['models']),10)
 def test_no_model_endpoint_auth_or_header_override(self):
  with tempfile.TemporaryDirectory() as d:
   p=model_catalog.select_guest(self.config(d))['prime_vault']['provider']
   for key,value in [('baseUrl','https://evil.invalid'),('headers',{'Authorization':'not-a-real-secret'}),('apiKey','not-a-real-secret')]:
    bad=copy.deepcopy(p);bad['model_definitions'][0][key]=value
    with self.assertRaises(ValueError):model_catalog.native_json(bad,str(uuid.uuid4()))
    with self.assertRaises(RuntimeError):guest.native_gateway(bad,str(uuid.uuid4()))
 def test_capability_substitution_and_duplicate_refused(self):
  with tempfile.TemporaryDirectory() as d:
   p=model_catalog.select_guest(self.config(d))['prime_vault']['provider'];p['max_tokens']=512
   with self.assertRaisesRegex(RuntimeError,'SUBSTITUTION'):guest.native_gateway(p,str(uuid.uuid4()))
   p=model_catalog.select_guest(self.config(d))['prime_vault']['provider'];p['model_definitions'].append(p['model_definitions'][0])
   with self.assertRaises(ValueError):model_catalog.native_json(p,str(uuid.uuid4()))
 def adapter(self,d):
  from test_catalog_lifecycle import StorageTests
  a,s,r=StorageTests().fixture(d,{'backend':'coordinator-local'});a.c['guest']=model_catalog.select_guest(self.config(d));a.c.update(job='fixture-job',task='worker',namespace='default');a.target=lambda:{'ID':a.c['allocation'],'Job':{'Version':1},'TaskStates':{'worker':{'Restarts':0,'StartedAt':'synthetic'}}};a.hash=workerctl.digest(workerctl.canonical(a.c));state=json.loads((a.local(s)/'session.json').read_text());state.update(config_hash=a.hash,phase='ready',attempt=a.attempt(a.target()));workerctl.atomic(a.local(s)/'session.json',state);a.require_initiator=lambda sid:None;return a,s
 def test_model_control_durable_once_and_unknown_blocks_work(self):
  with tempfile.TemporaryDirectory() as d:
   a,s=self.adapter(d);selection=str(uuid.uuid4());calls=[]
   def remote(s,action,**kw):
    calls.append((action,kw))
    yield {'type':'status','owner':{'status':'alive'}} if action=='status' else {'type':'model_selected','coordinator_value':kw['coordinator_value'],'worker_models_changed':False}
   a.remote=remote;row=a.select_model(s,selection,'gpt-6.1-sol');self.assertEqual(row['phase'],'completed');self.assertEqual(a.select_model(s,selection,'gpt-6.1-sol'),row);self.assertEqual([x[0] for x in calls],['status','select_model'])
   def lost(s,action,**kw):
    if action=='status':yield {'type':'status','owner':{'status':'alive'}}
    else:raise RuntimeError('lost control response')
   a.remote=lost;unknown=str(uuid.uuid4())
   with self.assertRaisesRegex(RuntimeError,'OUTCOME_UNKNOWN'):a.select_model(s,unknown,'gpt-6-luna')
   self.assertEqual(a.select_model(s,unknown,'gpt-6-luna')['phase'],'exec_unknown')
   import activity
   with self.assertRaisesRegex(RuntimeError,'UNKNOWN'):activity.pending(a,s)
 def test_wrong_owner_blocks_before_control(self):
  with tempfile.TemporaryDirectory() as d:
   a,s=self.adapter(d);a.require_initiator=mock.Mock(side_effect=RuntimeError('wrong owner'));a.remote=mock.Mock()
   with self.assertRaisesRegex(RuntimeError,'wrong owner'):a.select_model(s,str(uuid.uuid4()),'gpt-6.1-sol')
   a.remote.assert_not_called()
