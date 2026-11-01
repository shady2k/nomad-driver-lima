#!/usr/bin/env python3
"""One-shot composition of existing caller, node and publisher preflights."""
import json,pathlib,re,subprocess,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange,template_save as save

def preflight(packet,context,run=subprocess.run,receipts=None):
 save.validate_operation(packet)
 if save.sha(packet['plan'])!=packet['plan_sha256']:raise RuntimeError('PREFLIGHT_PLAN_CHANGED')
 evidence={};retained=bool(receipts and 'source_finish'in receipts)
 if retained:
  save.verify_stage('source_finish',receipts['source_finish'],packet);evidence['source']={'source_owner_current':True}
 for role in (('node','publisher')if retained else('source','node','publisher')):
  argv=context.get(role+'_preflight')
  if not isinstance(argv,list) or not argv or not argv[0].startswith('/') or any(not isinstance(x,str) or '\0'in x for x in argv):raise RuntimeError('FIXED_PREFLIGHT_BINDINGS_REQUIRED')
  result=run(argv,input=json.dumps({'stage':'preflight_retained_source'if retained and role=='node'else'preflight','packet':packet,'receipts':receipts or {}}),text=True,capture_output=True,timeout=120)
  if result.returncode or len(result.stdout)>1048576:raise RuntimeError('TEMPLATE_PREFLIGHT_FAILED_NO_SOURCE_STOP')
  row=json.loads(result.stdout)
  if row.get('operation_id')!=packet['plan']['operation_id'] or row.get('plan_sha256')!=packet['plan_sha256']:raise RuntimeError('UNBOUND_PREFLIGHT_RESULT')
  evidence[role]=row
 checks={'source_owner_current':evidence['source'].get('source_owner_current'),'isolated_builder_capacity_available':evidence['node'].get('isolated_builder_capacity_available'),'scoped_s3_writer_available':evidence['publisher'].get('scoped_s3_writer_available'),'catalog_cas_verified':evidence['publisher'].get('catalog_cas_verified'),'existing_pipeline_bound':evidence['node'].get('frozen_assets_staged') and evidence['publisher'].get('artifact_transport_binding_ready')}
 if any(v is not True for v in checks.values()):raise RuntimeError('TEMPLATE_PREFLIGHT_INCOMPLETE_NO_SOURCE_STOP')
 return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],**checks}
if __name__=='__main__':
 try:
  context=exchange.protected(sys.argv[1]);payload=json.load(sys.stdin)
  if payload.get('stage')not in (None,'preflight','preflight_retained_source'):raise RuntimeError('EXACT_PREFLIGHT_STAGE_REQUIRED')
  print(json.dumps(preflight(payload['packet'],context,receipts=payload.get('receipts')if payload.get('stage')=='preflight_retained_source'else None)))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
