import copy,importlib.util,pathlib,unittest,uuid
p=pathlib.Path(__file__).parents[2]/'deploy/provider-native-capabilities.py';spec=importlib.util.spec_from_file_location('provider_native',p);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class SingleProfileTests(unittest.TestCase):
 def caps(self):
  model={'id':'fixture-auto','name':'fixture-auto','api':'openai-completions','input':['text'],'reasoning':True,'contextWindow':1000000,'maxTokens':16384,'thinkingLevelMap':{'off':'none','low':'low'},'thinking':{'mode':'effort'},'compat':{'maxTokensField':'max_tokens','supportsReasoningEffort':True,'requiresReasoningContentOnAssistantMessages':True,'supportsReasoningParams':True}}
  return {'provider':{'id':'fixture','base_url':'https://gateway.invalid/v1'},'models':[model,{**model,'id':'not-enabled'}]}
 def test_single_profile_preserves_capabilities_no_fake_smoke_budget(self):
  c=self.caps();before=copy.deepcopy(c);g=m.guest_provider(c,'fixture-auto');n=m.native_profile(c,'fixture-auto',str(uuid.uuid4()));model=n['providers']['fixture']['models'][0]
  self.assertEqual(g['context_window'],1000000);self.assertEqual(g['max_tokens'],16384);self.assertEqual(model['contextWindow'],1000000);self.assertEqual(model['maxTokens'],16384);self.assertEqual(len(n['providers']['fixture']['models']),1);self.assertNotIn('thinking',model);self.assertNotIn('supportsReasoningParams',g['model_definition']['compat']);self.assertEqual(c,before)
 def test_old_cap_arguments_cannot_silently_change_model(self):
  with self.assertRaises(TypeError):m.native_profile(self.caps(),'fixture-auto',str(uuid.uuid4()),512,20000)
 def test_known_model_and_real_session_required(self):
  with self.assertRaises(ValueError):m.guest_provider(self.caps(),'unknown')
  with self.assertRaises(ValueError):m.native_profile(self.caps(),'fixture-auto','fake')
 def test_generated_config_works_with_existing_frozen_guest(self):
  spec=importlib.util.spec_from_file_location('frozen_task_main',pathlib.Path(__file__).parents[1]/'guest/task-main.py');guest=importlib.util.module_from_spec(spec);spec.loader.exec_module(guest)
  g=m.guest_provider(self.caps(),'fixture-auto');model,compat=guest.native_model(g);self.assertEqual(model['maxTokens'],16384);self.assertEqual(model['contextWindow'],1000000);self.assertTrue(compat['requiresReasoningContentOnAssistantMessages'])
