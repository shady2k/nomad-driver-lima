#!/usr/bin/env python3
"""Extract reviewed declarative provider metadata without executing JavaScript.

Supported input is the pinned model-capabilities.js declaration shape. Unexpected
expressions fail closed. Capabilities are not availability, price or spending grants.
"""
import argparse,hashlib,json,pathlib,re,subprocess,uuid

def frozen_body(text,name):
 m=re.search(r'const '+re.escape(name)+r' = Object\.freeze\(\{(.*?)\}\);',text,re.S)
 if not m:raise ValueError('Required frozen declarative object absent: '+name)
 return m.group(1)
def literals(body,known=None):
 result={};body=' '.join(line.split('//',1)[0]for line in body.splitlines());pos=0
 pattern=re.compile(r'\s*(?:\.\.\.([A-Za-z][A-Za-z0-9_]*)|([A-Za-z][A-Za-z0-9_]*):\s*(null|true|false|"[^"\\]*"|[0-9]+))\s*(?:,|$)')
 while body[pos:].strip():
  m=pattern.match(body,pos)
  if not m:raise ValueError('Nonliteral metadata declaration')
  if m[1]:
   if not known or m[1]not in known:raise ValueError('Unknown spread declaration')
   result.update(known[m[1]])
  else:result[m[2]]=json.loads(m[3])
  pos=m.end()
 return result
def thinking(levels):
 keys=list(levels);supported=[k for k in keys if levels[k] is not None];assert supported
 return {'mode':'effort','efforts':supported,'effortMap':{k:(levels[k]if levels[k]is not None else next((levels[q]for q in keys[i:]if levels[q]is not None),levels[supported[-1]]))for i,k in enumerate(keys)}}
def capabilities(text):
 glm=literals(frozen_body(text,'GLM_THINKING_LEVELS'));gpt=literals(frozen_body(text,'GPT_THINKING_LEVELS'));compat=literals(frozen_body(text,'OPENAI_COMPAT'))
 # These exact source declarations are structural guards, not executed expressions.
 assert 'const DEEPSEEK_THINKING_LEVELS = Object.freeze({ ...GLM_THINKING_LEVELS, off: "none" });'in text
 deep={**glm,'off':'none'}
 match=re.search(r'const openrouterDeepseekProfile = leveledProfile\(Object\.freeze\(\{(.*?)\}\), OPENAI_COMPAT\);',text,re.S);assert match
 router=literals(match[1],{'GLM_THINKING_LEVELS':glm})
 match=re.search(r'const \{(.*?)\.\.\.GPT_COMPAT\s*\} = OPENAI_COMPAT;',text,re.S);assert match
 removed=[x.strip().rstrip(',').strip()for x in match[1].splitlines()if x.strip()];assert all(x in compat for x in removed)
 gpt_compat={k:v for k,v in compat.items()if k not in removed}
 for name in ['glmProfile','deepseekProfile']:
  match=re.search(r'const '+name+r' = \(contextWindow, maxTokens\) => Object\.freeze\(\{(.*?)\}\);',text,re.S);assert match
  assert re.search(r'contextWindow,\s*maxTokens,\s*reasoning: true,',match[1])
 assert 'const gptProfile = leveledProfile(GPT_THINKING_LEVELS, GPT_COMPAT);'in text
 assert 'const gptProfileWithOff = leveledProfile(Object.freeze({ ...GPT_THINKING_LEVELS, off: "none" }), GPT_COMPAT);'in text
 factories={'glmProfile':(glm,compat),'deepseekProfile':(deep,compat),'openrouterDeepseekProfile':(router,compat),'gptProfile':(gpt,gpt_compat),'gptProfileWithOff':({**gpt,'off':'none'},gpt_compat)}
 block=re.search(r'export const MODEL_CAPABILITIES = Object\.freeze\(\{(.*?)\n\}\);',text,re.S);assert block
 models=[]
 for raw in block[1].splitlines():
  line=raw.strip()
  if not line or line.startswith('//'):continue
  m=re.fullmatch(r'"([^"\\]+)": ([A-Za-z][A-Za-z0-9_]*)\(([0-9]+), ([0-9]+)\),',line)
  if not m or m[2]not in factories:raise ValueError('Unknown model registry expression')
  levels,co=factories[m[2]];models.append({'id':m[1],'name':m[1],'api':'openai-completions','input':['text'],'reasoning':True,'contextWindow':int(m[3]),'maxTokens':int(m[4]),'thinkingLevelMap':dict(levels),'thinking':thinking(levels),'compat':dict(co)})
 assert models and len({m['id']for m in models})==len(models)
 return models

PRIME_COMPAT_BOOLEANS={'zaiToolStream','sendSessionAffinityHeaders','supportsStore','supportsDeveloperRole','supportsReasoningEffort','supportsUsageInStreaming','requiresToolResultName','requiresAssistantAfterToolResult','requiresThinkingAsText','requiresReasoningContentOnAssistantMessages','supportsStrictMode','supportsLongCacheRetention'}
PRIME_COMPAT_ENUMS={'maxTokensField':{'max_tokens','max_completion_tokens'},'thinkingFormat':{'openai','openrouter','deepseek','zai','qwen','qwen-chat-template'},'cacheControlFormat':{'anthropic'}}
EXPLICIT_OTHER_HOST_FIELDS={'supportsReasoningParams','clampOutputToModelMax','replayReasoningContent','requiresReasoningContentForAllAssistantTurns','requiresReasoningContentForToolCalls'}
def prime_definition(model):
 out={k:v for k,v in model.items()if k not in ('thinking','compat')};compat=model.get('compat',{});unknown=set(compat)-PRIME_COMPAT_BOOLEANS-set(PRIME_COMPAT_ENUMS)-EXPLICIT_OTHER_HOST_FIELDS
 if unknown:raise ValueError('Unreviewed native compat fields')
 out['compat']={k:v for k,v in compat.items()if k not in EXPLICIT_OTHER_HOST_FIELDS}
 for k,v in out['compat'].items():
  if k in PRIME_COMPAT_BOOLEANS:assert type(v)is bool
  else:assert v in PRIME_COMPAT_ENUMS[k]
 return out

def guest_provider(caps,model_id):
 """One accurate selected model; spending authorization is not model metadata."""
 found=[m for m in caps['models']if m['id']==model_id]
 if len(found)!=1:raise ValueError('Exact known model required')
 model=prime_definition(found[0])
 for k in ('maxTokens','contextWindow'):
  if type(model.get(k))is not int or not 1<=model[k]<=2000000:raise ValueError('Invalid source capability')
 return {'name':caps['provider']['id'],'base_url':caps['provider']['base_url'],'model':model_id,'max_tokens':model['maxTokens'],'context_window':model['contextWindow'],'model_definition':model}

def native_profile(caps,model_id,session_id):
 """Do not lower model capabilities for smoke; ACP has no per-turn token cap."""
 if str(uuid.UUID(session_id))!=session_id:raise ValueError('Actual canonical worker session required')
 selected=guest_provider(caps,model_id);model=dict(selected['model_definition']);compat=model.pop('compat',None)
 provider={'baseUrl':selected['base_url'],'apiKey':'LLM_API_KEY','api':'openai-completions','headers':{'x-opencode-session':session_id},'models':[model]}
 if compat is not None:provider['compat']=compat
 return {'providers':{selected['name']:provider}}

def guest_gateway(caps,coordinator_model):
 selected=guest_provider(caps,coordinator_model);selected.pop('model_definition',None)
 selected['model_definitions']=[prime_definition(m) for m in caps['models']]
 return selected

def native_gateway_profile(caps,coordinator_model,session_id):
 if str(uuid.UUID(session_id))!=session_id:raise ValueError('Actual canonical worker session required')
 selected=guest_gateway(caps,coordinator_model);providers={}
 for model in selected['model_definitions']:
  model=dict(model);compat=model.pop('compat',{});group=selected['name'][:80]+'-compat-'+hashlib.sha256(json.dumps(compat,sort_keys=True,separators=(',',':')).encode()).hexdigest()[:12]
  block=providers.setdefault(group,{'baseUrl':selected['base_url'],'apiKey':'LLM_API_KEY','api':'openai-completions','headers':{'x-opencode-session':session_id},'compat':compat,'models':[]})
  if block['compat']!=compat:raise ValueError('Compatibility group collision')
  block['models'].append(model)
 return {'providers':providers}

def main():
 p=argparse.ArgumentParser();p.add_argument('--repo',required=True);p.add_argument('--commit',required=True);p.add_argument('--output',required=True);p.add_argument('--repository-url',required=True);p.add_argument('--native-output');p.add_argument('--model');p.add_argument('--session');p.add_argument('--guest-provider-output');p.add_argument('--prime-definitions-output');p.add_argument('--guest-gateway-output');p.add_argument('--native-gateway-output');a=p.parse_args()
 assert re.fullmatch('[a-f0-9]{40}',a.commit)
 def read(name):return subprocess.check_output(['git','-C',a.repo,'show',a.commit+':'+name],text=True)
 src={name:read(name)for name in ['model-capabilities.js','gateway-models.js','session-headers.js']}
 m=re.search(r'export const PROVIDER_ID = "([^"\\]+)";',src['gateway-models.js']);assert m;provider=m[1]
 m=re.search(r'export const BASE_URL = "(https://[^"\\]+)";',src['gateway-models.js']);assert m;base=m[1]
 assert 'const OPENCODE_SESSION_HEADER = "x-opencode-session";'in src['session-headers.js']
 result={'schema_version':1,'source':{'repository':a.repository_url,'commit':a.commit,'files_sha256':{n:hashlib.sha256(v.encode()).hexdigest()for n,v in src.items()}},'provider':{'id':provider,'base_url':base,'key_env':'LLM_API_KEY','session_header':'x-opencode-session'},'models':capabilities(src['model-capabilities.js']),'availability_verified':False,'pricing_verified':False,'capabilities_are_not_task_budget':True,'javascript_executed':False}
 def save(path,value):
  raw=(json.dumps(value,sort_keys=True,indent=2)+'\n').encode();p=pathlib.Path(path)
  if p.exists():assert not p.is_symlink()and p.read_bytes()==raw,'Existing immutable output differs'
  else:
   with p.open('xb')as f:f.write(raw)
   p.chmod(0o444)
  return hashlib.sha256(raw).hexdigest()
 digest=save(a.output,result)
 if a.prime_definitions_output:save(a.prime_definitions_output,{'schema_version':1,'source':result['source'],'models':[prime_definition(m)for m in result['models']],'projected_out_fields':sorted(EXPLICIT_OTHER_HOST_FIELDS|{'thinking'}),'availability_verified':False,'capabilities_are_not_task_budget':True})
 if a.guest_gateway_output:
  assert a.model,'Explicit coordinator model required'
  save(a.guest_gateway_output,guest_gateway(result,a.model))
 if a.native_gateway_output:
  assert a.model and a.session,'Explicit coordinator and actual session required'
  save(a.native_gateway_output,native_gateway_profile(result,a.model,a.session))
 if a.guest_provider_output:
  assert a.model,'Explicit selected model required'
  save(a.guest_provider_output,guest_provider(result,a.model))
 if a.native_output:
  assert a.model and a.session,'Explicit model and session required'
  save(a.native_output,native_profile(result,a.model,a.session))
 print(json.dumps({'models':len(result['models']),'commit':a.commit,'sha256':digest,'output':a.output,'javascript_executed':False,'availability_verified':False,'plugin_monetary_approval_required':False,'acp_per_turn_output_cap_supported':False}))
if __name__=='__main__':main()
