import hashlib,json,pathlib,sys,tempfile,unittest
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import model_catalog
class ModelCatalogTests(unittest.TestCase):
 def config(self,d):
  m={'id':'gpt-6.1-sol','name':'gpt-6.1-sol','api':'openai-completions','input':['text'],'reasoning':True,'contextWindow':272000,'maxTokens':128000,'compat':{'supportsStore':False},'thinkingLevelMap':{'off':None,'high':'high'}};doc={'schema_version':1,'source':{'commit':'f'*40},'models':[m]};p=pathlib.Path(d)/'models';raw=json.dumps(doc).encode();p.write_bytes(raw);return {'model_catalog_file':str(p),'model_catalog_sha256':hashlib.sha256(raw).hexdigest()},p
 def test_exact_listing_is_not_runtime_availability_or_money_gate(self):
  with tempfile.TemporaryDirectory() as d:
   c,p=self.config(d);out=model_catalog.listing(c);self.assertFalse(out['plugin_monetary_approval_required']);self.assertFalse(out['runtime_enablement_verified']);self.assertTrue(out['models'][0]['native_definition_compatible']);self.assertNotIn('worker_selectable_in_native_catalog',out['models'][0]);self.assertEqual(model_catalog.exact(c,'gpt-6.1-sol')['id'],'gpt-6.1-sol')
   with self.assertRaises(ValueError):model_catalog.exact(c,'sol')
 def test_pin_mismatch_fails_and_unsupported_reason_is_explicit(self):
  with tempfile.TemporaryDirectory() as d:
   c,p=self.config(d);p.write_text('{}')
   with self.assertRaisesRegex(RuntimeError,'CHECKSUM'):model_catalog.listing(c)
   self.assertEqual(model_catalog.reasons({'id':'x','thinking':{}}),['unsupported_native_model_fields'])
 def test_guidance_no_required_money_question_and_role_independence(self):
  text=(pathlib.Path(__file__).parents[1]/'skills/nomad-workers/SKILL.md').read_text();self.assertIn('do not ask for a dollar amount',text);self.assertNotIn('verify the authorized monetary',text);self.assertIn('Coordinator selection and native Prime RLM worker selections are independent',text)
