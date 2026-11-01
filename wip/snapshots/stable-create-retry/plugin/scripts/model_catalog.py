"""Approved metadata listing only. Does not enable routes or contact a model."""
import copy,hashlib,json,pathlib,re,urllib.parse,uuid
ALLOWED={'id','name','api','input','reasoning','contextWindow','maxTokens','thinkingLevelMap','compat'}
BOOLS={'zaiToolStream','sendSessionAffinityHeaders','supportsStore','supportsDeveloperRole','supportsReasoningEffort','supportsUsageInStreaming','requiresToolResultName','requiresAssistantAfterToolResult','requiresThinkingAsText','requiresReasoningContentOnAssistantMessages','supportsStrictMode','supportsLongCacheRetention'}
ENUMS={'maxTokensField':{'max_tokens','max_completion_tokens'},'thinkingFormat':{'openai','openrouter','deepseek','zai','qwen','qwen-chat-template'},'cacheControlFormat':{'anthropic'}}
def reasons(m):
 if not isinstance(m,dict) or set(m)-ALLOWED:return ['unsupported_native_model_fields']
 if m.get('api')!='openai-completions' or m.get('input')!=['text'] or type(m.get('reasoning'))!=bool:return ['unsupported_native_model_shape']
 if any(type(m.get(k))!=int or not 1<=m[k]<=2000000 for k in ('contextWindow','maxTokens')):return ['invalid_capability_ceiling']
 c=m.get('compat',{})
 if not isinstance(c,dict) or set(c)-BOOLS-set(ENUMS):return ['unsupported_native_compat_fields']
 if any(type(v)!=bool if k in BOOLS else not isinstance(v,str) or v not in ENUMS[k] for k,v in c.items()):return ['invalid_native_compat_value']
 levels=m.get('thinkingLevelMap',{})
 if not isinstance(levels,dict) or set(levels)-{'off','minimal','low','medium','high','xhigh','max'} or any(v is not None and (not isinstance(v,str) or not v or len(v)>64) for v in levels.values()):return ['invalid_thinking_levels']
 return []
def load(config):
 path=config.get('model_catalog_file');sha=config.get('model_catalog_sha256')
 if not path or not isinstance(sha,str) or not re.fullmatch('[a-f0-9]{64}',sha):raise RuntimeError('APPROVED_MODEL_CATALOG_NOT_CONFIGURED')
 raw=pathlib.Path(path).read_bytes()
 if len(raw)>4194304 or hashlib.sha256(raw).hexdigest()!=sha:raise RuntimeError('MODEL_CATALOG_CHECKSUM_MISMATCH')
 doc=json.loads(raw);models=doc.get('models');source=doc.get('source',{})
 if doc.get('schema_version')!=1 or not isinstance(models,list) or not 1<=len(models)<=256 or not re.fullmatch('[a-f0-9]{40}',source.get('commit','')):raise RuntimeError('MODEL_CATALOG_INVALID')
 seen=set()
 for m in models:
  id=m.get('id') if isinstance(m,dict) else None
  if not isinstance(id,str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}',id) or id in seen:raise RuntimeError('MODEL_ID_INVALID_OR_DUPLICATE')
  seen.add(id)
 return doc

def group_name(gateway,compat):
 if not isinstance(gateway,str) or not re.fullmatch('[-A-Za-z0-9_.]{1,128}',gateway):raise ValueError('GATEWAY_NAME_INVALID')
 return gateway[:80]+'-compat-'+hashlib.sha256(json.dumps(compat,sort_keys=True,separators=(',',':')).encode()).hexdigest()[:12]

def definitions(provider):
 models=provider.get('model_definitions')
 if not isinstance(models,list) or not 1<=len(models)<=64:raise ValueError('NATIVE_CATALOG_REQUIRED')
 seen=set()
 for m in models:
  if reasons(m) or not isinstance(m.get('id'),str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}',m['id']) or m['id'] in seen:raise ValueError('NATIVE_CATALOG_INVALID_OR_DUPLICATE')
  seen.add(m['id'])
 return models

def selectors(provider):
 return {m['id']:{'provider':group_name(provider['name'],m.get('compat',{})),'model':m['id'],'coordinator_acp_value':json.dumps([group_name(provider['name'],m.get('compat',{})),m['id']],separators=(',',':')),'worker_selector':group_name(provider['name'],m.get('compat',{}))+'/'+m['id']} for m in definitions(provider)}

def listing(config):
 doc=load(config);provider=config.get('guest',{}).get('prime_vault',{}).get('provider',{});gateway=provider.get('name');out=[]
 for m in doc['models']:
  why=reasons(m);row={'id':m['id'],'context_window':m.get('contextWindow'),'max_output_tokens':m.get('maxTokens'),'native_definition_compatible':not why,'unsupported_reasons':why,'configured_coordinator_default':provider.get('model')==m['id'],'reasoning':m.get('reasoning'),'thinking_levels':[k for k,v in m.get('thinkingLevelMap',{}).items() if v is not None],'availability_verified':False}
  if not why and gateway:
   group=group_name(gateway,m.get('compat',{}));row.update(coordinator_acp_value=json.dumps([group,m['id']],separators=(',',':')),worker_selector=group+'/'+m['id'],compatibility_group_internal=True)
  out.append(row)
 return {'gateway':gateway,'source_commit':doc['source']['commit'],'models':out,'plugin_monetary_approval_required':False,'role_selection':'coordinator ACP model option; each worker native rlm.spawn model independently; omitted worker model inherits parent','runtime_enablement_verified':False,'model_aliases_added':False}

def select_guest(config,model=None):
 doc=load(config);guest=copy.deepcopy(config['guest']);provider=guest['prime_vault']['provider'];model=model or provider['model'];found=[m for m in doc['models'] if m['id']==model]
 if len(found)!=1 or reasons(found[0]):raise ValueError('EXACT_SUPPORTED_MODEL_REQUIRED')
 provider.pop('model_definition',None);provider['model_definitions']=[copy.deepcopy(m) for m in doc['models'] if not reasons(m)]
 provider.update(model=model,max_tokens=found[0]['maxTokens'],context_window=found[0]['contextWindow']);guest['model']=None
 contract=guest.setdefault('runtime_contract',{})
 for key in ('approved_total_usd','output_token_budget','context_token_budget','spending_policy','observed_input_usd_per_million','observed_output_usd_per_million'):contract.pop(key,None)
 contract.update(model=model,model_catalog_source_commit=doc['source']['commit'],plugin_monetary_approval_required=False,worker_model_selection='native_rlm_per_worker')
 return guest

def native_json(provider,session):
 if str(uuid.UUID(session))!=session:raise ValueError('SESSION_UUID_REQUIRED')
 u=urllib.parse.urlsplit(provider['base_url'])
 if u.scheme!='https' or not u.hostname or u.username or u.password or u.query or u.fragment:raise ValueError('FIXED_HTTPS_GATEWAY_REQUIRED')
 providers={};mapping=selectors(provider)
 for m in definitions(provider):
  group=mapping[m['id']]['provider'];compat=copy.deepcopy(m.get('compat',{}));p=providers.setdefault(group,{'baseUrl':provider['base_url'],'apiKey':'LLM_API_KEY','api':'openai-completions','headers':{'x-opencode-session':session},'compat':compat,'models':[]})
  if p['compat']!=compat:raise ValueError('COMPAT_GROUP_COLLISION')
  item=copy.deepcopy(m);item.pop('compat',None);p['models'].append(item)
 return {'providers':providers}

def worker_selector(provider,model=None):
 # None is deliberately forwarded to native Prime inheritance, not copied from coordinator.
 if model is None:return None
 rows=selectors(provider)
 if model not in rows:raise ValueError('EXACT_SUPPORTED_MODEL_REQUIRED')
 return rows[model]['worker_selector']

def exact(config,id):
 rows=[m for m in listing(config)['models'] if m['id']==id]
 if len(rows)!=1:raise ValueError('EXACT_APPROVED_MODEL_ID_REQUIRED')
 return rows[0]
