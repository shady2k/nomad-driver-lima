#!/usr/bin/env python3
"""Existing caller-side result/finish contract, not a new cleanup protocol.
Run as the actual caller identity with its existing config and native owner state.
"""
import json,pathlib,re,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save
from workerctl import Adapter

def preflight(adapter,packet):
 p=packet['plan'];s=p['source'];session=s['session_id']
 if save.sha(p)!=packet['plan_sha256'] or adapter.hash!=s['config_sha256']:raise RuntimeError('SOURCE_PREFLIGHT_CONFIG_CHANGED')
 adapter.require_initiator(session);state=adapter.load(session);allocation=adapter.target()
 if state.get('phase')!='ready' or adapter.attempt(allocation)!=s['attempt'] or allocation.get('ID')!=s['allocation']:raise RuntimeError('SOURCE_PREFLIGHT_ATTEMPT_CHANGED')
 import activity
 activity.pending(adapter,session)
 snapshot=p['snapshot'];manifest=adapter.result(session,snapshot['request_id'],snapshot['snapshot_id']);activity.require_current(adapter,session,manifest,snapshot['request_id'],snapshot['snapshot_id'])
 if save.sha(manifest)!=snapshot['manifest_sha256'] or not adapter.c.get('vault'):raise RuntimeError('SOURCE_PREFLIGHT_RECEIPT_OR_VAULT_CHANGED')
 return {'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'source_owner_current':True}

def finish(adapter,packet):
 p=packet['plan'];source=p['source'];session=source['session_id'];snapshot=p['snapshot']
 if save.sha(p)!=packet['plan_sha256'] or adapter.hash!=source['config_sha256'] or any(adapter.c.get(k)!=source[k] for k in ('allocation','job','task')):raise RuntimeError('SOURCE_FINISH_BINDING_CHANGED')
 if p.get('source_recovery'):
  receipt=save.recovery_contract(p['source_recovery']['receipt'],source)
  if save.sha(receipt)!=p['source_recovery']['receipt_sha256']:raise RuntimeError('RECOVERY_RECEIPT_CHANGED')
  preflight(adapter,packet)
  return {'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'source_allocation':source['allocation'],'source_attempt':source['attempt'],'source_owner_verified':True,'results_receipt_verified':True,'source_recovery_receipt_verified':True,'source_vm_stopped':True,'source_disk_retained':True,'revocation_unconfirmed':True,'finish_replayed':False}
 if not adapter.c.get('vault'):raise RuntimeError('SOURCE_VAULT_CLEANUP_CONTRACT_REQUIRED')
 adapter.require_initiator(session)
 if adapter.load(session).get('attempt')!=source['attempt']:raise RuntimeError('SOURCE_FINISH_ATTEMPT_CHANGED')
 manifest=adapter.result(session,snapshot['request_id'],snapshot['snapshot_id'])
 if save.sha(manifest)!=snapshot['manifest_sha256']:raise RuntimeError('SOURCE_RESULT_SNAPSHOT_CHANGED')
 state=adapter.finish(session,snapshot['request_id'],snapshot['snapshot_id'])
 if state.get('phase')!='nomad_terminal' or state.get('disk_purge_requested')is not False:raise RuntimeError('SOURCE_FINISH_NOT_CONFIRMED_DISK_RETAINED')
 # Existing finish checks exact allocation attempt, terminal/desired-stop,
 # token_revoked + vm_stopped driver events for Vault tasks. Do not invent proof
 # of token revocation for a caller configured without that contract.
 if not adapter.c.get('vault'):raise RuntimeError('SOURCE_VAULT_CLEANUP_CONTRACT_REQUIRED')
 return {'operation_id':p['operation_id'],'plan_sha256':packet['plan_sha256'],'source_allocation':source['allocation'],'source_attempt':source['attempt'],'source_owner_verified':True,'results_receipt_verified':True,'prime_bao_stopped':True,'task_token_revoked':True,'source_vm_stopped':True,'source_disk_retained':True}
if __name__=='__main__':
 try:
  payload=json.load(sys.stdin)
  if payload.get('stage')not in ('preflight','source_finish'):raise RuntimeError('EXACT_SOURCE_STAGE_REQUIRED')
  print(json.dumps((preflight if payload['stage']=='preflight' else finish)(Adapter(sys.argv[1]),payload['packet'])))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
