#!/usr/bin/env python3
"""Bounded optional probe through the EXISTING S3 exchange identity.
Creates one <=1KiB disposable object, never touches catalog/bases or deletes data.
Run only after approval of the exact unique probe key; no grants are created.
"""
import concurrent.futures,hashlib,json,pathlib,re,sys,uuid
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import exchange

def probe(store,key):
 if not re.fullmatch(r'environments/\.cas-probes/[a-f0-9-]{36}/probe.json',key) or str(uuid.UUID(key.split('/')[2]))!=key.split('/')[2]:raise ValueError('EXACT_UNIQUE_CAS_PROBE_KEY_REQUIRED')
 initial=b'{"probe":"initial"}\n'
 # Create must be conditional: existing key is never re-used or overwritten.
 store.conditional_put(key,initial)
 if store.get(key)!=initial:raise RuntimeError('CAS_PROBE_INITIAL_READBACK_FAILED')
 try:store.conditional_put(key,b'{"probe":"must-not-write"}\n')
 except Exception:pass
 else:raise RuntimeError('IF_NONE_MATCH_NOT_ENFORCED_PROBE_RETAINED')
 if store.get(key)!=initial:raise RuntimeError('IF_NONE_MATCH_CHANGED_DATA_PROBE_RETAINED')
 etag=store.stat(key)['etag'];payloads=[b'{"probe":"winner-a"}\n',b'{"probe":"winner-b"}\n']
 def write(raw):
  try:store.conditional_put(key,raw,etag);return True
  except Exception:return False
 with concurrent.futures.ThreadPoolExecutor(2) as pool:results=list(pool.map(write,payloads))
 final=store.get(key)
 if sum(results)!=1 or final!=payloads[results.index(True)]:raise RuntimeError('CAS_RACE_UNKNOWN_OR_UNSUPPORTED_PROBE_RETAINED')
 # ETag from the old value must now fail, even without a concurrent writer.
 try:store.conditional_put(key,initial,etag)
 except Exception:pass
 else:raise RuntimeError('STALE_IF_MATCH_NOT_ENFORCED_PROBE_RETAINED')
 if store.get(key)!=final:raise RuntimeError('STALE_IF_MATCH_CHANGED_DATA_PROBE_RETAINED')
 binding=hashlib.sha256(json.dumps({k:store.cfg[k]for k in ('endpoint','bucket','region')},sort_keys=True,separators=(',',':')).encode()).hexdigest() if hasattr(store,'cfg') else None
 return {'store_binding_sha256':binding,'conditional_catalog_writes_verified':True,'probe_key':key,'if_none_match_verified':True,'competing_if_match_one_winner':True,'stale_if_match_verified':True,'readback_sha256':hashlib.sha256(final).hexdigest(),'probe_retained':True,'catalog_untouched':True}
if __name__=='__main__':
 try:
  # Private operator policy pins the exact key approved for this invocation.
  policy=exchange.protected(sys.argv[1]);key=sys.argv[2]
  if policy.get('approved_cas_probe_key')!=key:raise RuntimeError('EXACT_OPERATOR_PROBE_APPROVAL_REQUIRED')
  print(json.dumps(probe(exchange.Store(exchange.config(policy['exchange_config'])),key)))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
