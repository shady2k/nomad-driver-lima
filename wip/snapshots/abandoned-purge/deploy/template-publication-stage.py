#!/usr/bin/env python3
"""Finite binding to existing artifact publisher and conditional catalog append.
Executed only by the configured operator S3 writer, never inside a worker guest.
"""
import hashlib,importlib.util,json,pathlib,re,subprocess,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save as save
spec=importlib.util.spec_from_file_location('artifact_publisher',pathlib.Path(__file__).with_name('publish-artifact.py'));publisher=importlib.util.module_from_spec(spec);spec.loader.exec_module(publisher)
def preflight(packet,context,run=subprocess.run):
 proof=exchange.protected(context['cas_proof_file']);cfg=exchange.config(context['exchange_config']);binding=hashlib.sha256(save.canonical({k:cfg[k]for k in ('endpoint','bucket','region')})).hexdigest()
 if proof.get('store_binding_sha256')!=binding or not all(proof.get(k)is True for k in ('conditional_catalog_writes_verified','if_none_match_verified','competing_if_match_one_winner','stale_if_match_verified')):raise RuntimeError('VERIFIED_BOUND_CAS_PROOF_REQUIRED')
 store=exchange.Store(cfg)
 try:raw=store.get(context['catalog_key'])
 except exchange.ExchangeError as e:
  if str(e)!='exchange_s3_http_404':raise
  seed=pathlib.Path(context['initial_catalog_file']);raw=seed.read_bytes()
  if context.get('expected_catalog_sha256')is not None or hashlib.sha256(raw).hexdigest()!=context.get('initial_catalog_sha256'):raise RuntimeError('PINNED_EXISTING_CATALOG_SEED_REQUIRED')
 else:
  if hashlib.sha256(raw).hexdigest()!=context.get('expected_catalog_sha256'):raise RuntimeError('CURRENT_CATALOG_PIN_CHANGED')
 doc=json.loads(raw)
 if doc.get('schema_version')!=1 or not doc.get('templates'):raise RuntimeError('EXISTING_BASE_VERSIONS_MUST_BE_PRESERVED')
 command=context.get('artifact_fetch_command')
 if not isinstance(command,list)or not command or not command[0].startswith('/'):raise RuntimeError('FIXED_ARTIFACT_FETCH_BINDING_REQUIRED')
 result=run(command,input=json.dumps({'action':'probe','packet':packet}),text=True,capture_output=True,timeout=30)
 if result.returncode:raise RuntimeError('ARTIFACT_TRANSPORT_PREFLIGHT_FAILED')
 row=json.loads(result.stdout)
 if row.get('plan_sha256')!=packet['plan_sha256']or row.get('operation_id')!=packet['plan']['operation_id']or row.get('artifact_transport_binding_ready')is not True:raise RuntimeError('UNBOUND_ARTIFACT_TRANSPORT_PREFLIGHT')
 return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],'catalog_cas_verified':True,'scoped_s3_writer_available':True,'artifact_transport_binding_ready':True}

def stage(name,packet,receipts,context,run=subprocess.run,store_factory=None):
 if name not in ('publish','catalog_commit'):raise RuntimeError('PUBLICATION_STAGE_REQUIRED')
 if save.sha(packet['plan'])!=packet['plan_sha256']:raise RuntimeError('PUBLICATION_PLAN_CHANGED')
 p=packet['plan']
 if name=='publish':
  import importlib.util
  spec=importlib.util.spec_from_file_location('accepted_transfer',pathlib.Path(__file__).with_name('template-artifact-transfer.py'));transport=importlib.util.module_from_spec(spec);spec.loader.exec_module(transport);artifact=transport.transfer(packet,receipts,context)
 else:artifact=pathlib.Path(context['artifact_root'])/p['operation_id']/'artifact'
 if artifact.is_symlink() or not artifact.is_dir():raise RuntimeError('ACCEPTED_ARTIFACT_ROOT_REQUIRED')
 entry=json.loads((artifact/'catalog.json').read_text())['templates'][0]
 if (entry['template_id'],entry['version'])!=(p['template']['template_id'],p['template']['version']):raise RuntimeError('EXACT_PUBLICATION_TEMPLATE_REQUIRED')
 export=save.verify_stage('export',receipts['export'],packet);save.verify_stage('check',receipts['check'],packet);save.verify_stage('catalog',receipts['catalog'],packet)
 if entry['disk']['sha256']!=export['disk_sha256']:raise RuntimeError('PUBLICATION_DISK_CHANGED')
 prefix='environments/'+entry['template_id']+'/'+entry['version']+'/'+p['operation_id'];result={'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'disk_sha256':entry['disk']['sha256']}
 if name=='publish':
  args=[context['python'],str(pathlib.Path(__file__).with_name('publish-artifact.py')),'--config',context['exchange_config'],'--artifact',str(artifact),'--prefix',prefix]
  completed=run(args,text=True,capture_output=True,timeout=context.get('publish_timeout_seconds',3600))
  if completed.returncode:raise RuntimeError('PUBLICATION_OUTCOME_UNKNOWN_RETAIN_ARTIFACTS')
  proof=json.loads((artifact/'publish-receipt.json').read_text());expected={('disk.raw' if entry['disk']['format']=='raw' else 'disk.qcow2'):entry['disk']['sha256'],'runtime.yaml':entry['runtime_yaml']['sha256']}
  if proof.get('prefix')!=prefix or any(not any(o.get('key')==prefix+'/'+file and o.get('sha256')==sha for o in proof.get('objects',[])) for file,sha in expected.items()):raise RuntimeError('PUBLICATION_READBACK_PROOF_INCOMPLETE')
  result['all_objects_readback_verified']=True
 else:
  save.verify_stage('publish',receipts['publish'],packet)
  proof=exchange.protected(context['cas_proof_file'])
  if not all(proof.get(k)is True for k in ('conditional_catalog_writes_verified','if_none_match_verified','competing_if_match_one_winner','stale_if_match_verified','catalog_untouched')):raise RuntimeError('VERIFIED_CATALOG_CAS_PROOF_REQUIRED')
  cfg=exchange.config(context['exchange_config']);binding=hashlib.sha256(save.canonical({k:cfg[k]for k in ('endpoint','bucket','region')})).hexdigest()
  if proof.get('store_binding_sha256')!=binding:raise RuntimeError('CAS_PROOF_STORE_BINDING_CHANGED')
  store=(store_factory or exchange.Store)(cfg)
  # Artifact locations remain explicit nonsecret operator cache/read locators.
  # This never makes private objects public or adds credentials to URLs.
  for key in ('disk','runtime_yaml'):
   if context.get('catalog_urls',{}).get(key):entry[key]['url']=context['catalog_urls'][key].format(operation_id=p['operation_id'],disk_sha256=entry['disk']['sha256'],yaml_sha256=entry['runtime_yaml']['sha256'])
  seed=None
  if context.get('expected_catalog_sha256')is None:
   path=pathlib.Path(context['initial_catalog_file']);raw=path.read_bytes()
   if path.is_symlink() or hashlib.sha256(raw).hexdigest()!=context.get('initial_catalog_sha256'):raise RuntimeError('PINNED_INITIAL_CATALOG_SEED_REQUIRED')
   seed=json.loads(raw)
  result.update(publisher.append_catalog(store,context['catalog_key'],context['expected_catalog_sha256'],entry,True,seed))
 return save.verify_stage(name,result,packet)
if __name__=='__main__':
 try:
  context=exchange.protected(sys.argv[1]);payload=json.load(sys.stdin);print(json.dumps(preflight(payload['packet'],context)if payload['stage']=='preflight'else stage(payload['stage'],payload['packet'],payload['receipts'],context)))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
