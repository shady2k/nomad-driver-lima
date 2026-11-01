import copy,importlib.util,json,pathlib,unittest
spec=importlib.util.spec_from_file_location('native_task_main',pathlib.Path(__file__).parents[1]/'guest/task-main.py');runtime=importlib.util.module_from_spec(spec);spec.loader.exec_module(runtime)
class NativeModelTests(unittest.TestCase):
 def provider(self,budget=32768):
  return {'name':'fixture','model':'fixture-auto','base_url':'https://gateway.invalid/v1','max_tokens':budget,'context_window':128000,'model_definition':{'id':'fixture-auto','name':'fixture-auto','api':'openai-completions','input':['text'],'reasoning':True,'maxTokens':384000,'contextWindow':1000000,'thinkingLevelMap':{'off':'none','minimal':None,'low':'low','high':'high'},'compat':{'maxTokensField':'max_tokens','supportsReasoningEffort':True,'supportsDeveloperRole':False,'supportsStore':False,'thinkingFormat':'openai','requiresReasoningContentOnAssistantMessages':True}}}
 def test_native_json_preserves_reasoning_and_provider_compat_with_separate_budgets(self):
  p=self.provider();original=copy.deepcopy(p);m,c=runtime.native_model(p)
  profile=json.loads(json.dumps({'providers':{'fixture':{'models':[m],'compat':c}}}))
  self.assertEqual(profile['providers']['fixture']['models'][0]['thinkingLevelMap'],p['model_definition']['thinkingLevelMap']);self.assertTrue(m['reasoning']);self.assertTrue(c['requiresReasoningContentOnAssistantMessages']);self.assertEqual(m['maxTokens'],32768);self.assertEqual(m['contextWindow'],128000);self.assertEqual(p,original)
  smoke,_=runtime.native_model(self.provider(128));self.assertEqual(smoke['maxTokens'],128);self.assertEqual(m['maxTokens'],32768)
 def test_reject_override_unknown_and_unsupported_host_fields(self):
  for key,value in [('baseUrl','https://evil.invalid'),('headers',{'Authorization':'synthetic'}),('apiKey','synthetic'),('thinking',{'mode':'effort'}),('code','exec')]:
   p=self.provider();p['model_definition'][key]=value
   with self.assertRaisesRegex(RuntimeError,'FIELD_UNSUPPORTED'):runtime.native_model(p)
  for key in ['supportsReasoningParams','clampOutputToModelMax','replayReasoningContent','requiresReasoningContentForAllAssistantTurns','openRouterRouting']:
   p=self.provider();p['model_definition']['compat'][key]=True
   with self.assertRaisesRegex(RuntimeError,'COMPAT_FIELD_UNSUPPORTED'):runtime.native_model(p)
 def test_invalid_shapes_budget_and_identity_fail_closed(self):
  variants=[('id','other'),('api','other'),('input',['image']),('reasoning','true'),('maxTokens',32),('contextWindow',1),('thinkingLevelMap',{'ultra':'max'}),('thinkingLevelMap',{'low':''}),('compat',{'supportsStore':'false'}),('compat',{'thinkingFormat':'unknown'})]
  for k,v in variants:
   p=self.provider();p['model_definition'][k]=v
   with self.assertRaises(RuntimeError,msg=k):runtime.native_model(p)
  for n in [True,0,-1,2000001]:
   p=self.provider();p['max_tokens']=n
   with self.assertRaises(RuntimeError):runtime.native_model(p)
 def test_legacy_config_still_nonreasoning(self):
  p=self.provider();del p['model_definition'];m,c=runtime.native_model(p);self.assertFalse(m['reasoning']);self.assertIsNone(c)
if __name__=='__main__':unittest.main()
