#!/usr/bin/env python3
"""Publish an accepted boot artifact using the existing trusted exchange identity.
No credentials or signed URLs are written. Existing objects are verified, never replaced.
"""
import argparse,hashlib,json,pathlib,sys,urllib.request,urllib.error,uuid

def append_catalog(store,key,expected_sha256,entry,conditional_writes_verified=False,initial_catalog=None):
 """Atomic append via existing S3 store. No overwrite of a version/platform tuple."""
 if not conditional_writes_verified:raise RuntimeError('CATALOG_CAS_SUPPORT_UNVERIFIED')
 import urllib.parse,re
 for k in ('template_id','version','architecture','vm_type'):
  if not isinstance(entry.get(k),str) or not re.fullmatch('[a-z0-9][a-z0-9._-]{0,79}',entry[k]):raise ValueError('CATALOG_ENTRY_IDENTITY')
 for k in ('disk','runtime_yaml'):
  u=urllib.parse.urlsplit(entry[k]['url'])
  if u.scheme not in ('file','https') or u.username or u.password or u.query or u.fragment:raise ValueError('CATALOG_SECRET_OR_EPHEMERAL_URL')
 if entry.get('acceptance',{}).get('sealed')is not True or entry['acceptance'].get('independent_boots',0)<2 or entry['acceptance'].get('unique_identities')is not True:raise ValueError('CATALOG_ACCEPTANCE_REQUIRED')
 absent=False
 try:raw=store.get(key)
 except Exception as e:
  if str(e)!='exchange_s3_http_404':raise
  if expected_sha256 is not None or not isinstance(initial_catalog,dict):raise RuntimeError('CATALOG_INITIAL_SEED_REQUIRED_NO_EMPTY_REPLACEMENT') from None
  absent=True;raw=(json.dumps(initial_catalog,sort_keys=True,separators=(',',':'))+'\n').encode()
 current=hashlib.sha256(raw).hexdigest();doc=json.loads(raw)
 if doc.get('schema_version')!=1 or not isinstance(doc.get('templates'),list):raise ValueError('CATALOG_SCHEMA')
 ident=lambda e:tuple(e[k]for k in ('template_id','version','architecture','vm_type'))
 previous=[e for e in doc['templates'] if ident(e)==ident(entry)]
 if previous:
  if len(previous)!=1 or previous[0]!=entry:raise RuntimeError('CATALOG_VERSION_EXISTS_NO_OVERWRITE')
  if absent:raise RuntimeError('NEW_ENTRY_ALREADY_IN_SEED_REVIEW_REQUIRED')
  return {'catalog_sha256':current,'idempotent':True,'atomic_conditional_append':True,'new_version_no_overwrite':True,'disk_sha256':entry['disk']['sha256']}
 if not absent and current!=expected_sha256:raise RuntimeError('CATALOG_CHANGED_REPLAN_NO_OVERWRITE')
 etag=None if absent else store.stat(key)['etag']
 # Re-read under the ETag observation; concurrent mutation still loses CAS.
 if not absent and hashlib.sha256(store.get(key)).hexdigest()!=current:raise RuntimeError('CATALOG_CHANGED_BEFORE_CAS')
 doc['templates'].append(entry);target=(json.dumps(doc,sort_keys=True,separators=(',',':'))+'\n').encode();wanted=hashlib.sha256(target).hexdigest()
 try:store.conditional_put(key,target,etag)
 except Exception:
  # A lost response is reconciled by exact readback, never another PUT.
  if hashlib.sha256(store.get(key)).hexdigest()!=wanted:raise RuntimeError('CATALOG_COMMIT_UNKNOWN_OR_CONFLICT_RETAIN_ARTIFACTS') from None
 if hashlib.sha256(store.get(key)).hexdigest()!=wanted:raise RuntimeError('CATALOG_READBACK_MISMATCH')
 return {'catalog_sha256':wanted,'idempotent':False,'atomic_conditional_append':True,'new_version_no_overwrite':True,'disk_sha256':entry['disk']['sha256']}

def main():
 p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--artifact',required=True);p.add_argument('--prefix',required=True);p.add_argument('--files',nargs='+',choices=['disk.qcow2','disk.raw','runtime.yaml','metadata.json','acceptance.json']);a=p.parse_args()
 sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]/'plugin/scripts'))
 import exchange
 root=pathlib.Path(a.artifact).resolve();accept=json.loads((root/'acceptance.json').read_text())
 assert accept['sealed'] and accept['independent_boots']==2 and accept['unique_identities']
 # Caller creates a unique new immutable version prefix, not a mutable latest key.
 assert str(uuid.UUID(a.prefix.split('/')[-1]))==a.prefix.split('/')[-1]
 entry=json.loads((root/'catalog.json').read_text())['templates'][0]
 parts=a.prefix.split('/')
 assert (len(parts)==3 and parts[:2]==['environments','agent-base']) or (len(parts)==4 and parts[:3]==['environments',entry['template_id'],entry['version']]),'Exact immutable template/version prefix required'
 store=exchange.Store(exchange.config(a.config))
 def digest(path):
  h=hashlib.sha256()
  with path.open('rb') as f:
   for b in iter(lambda:f.read(8*1024*1024),b''):h.update(b)
  return h.hexdigest()
 receipt={'storage_kind':'s3','bucket':store.cfg['bucket'],'prefix':a.prefix,'objects':[]}
 for name in (a.files or [('disk.raw' if entry['disk']['format']=='raw' else 'disk.qcow2'),'runtime.yaml','metadata.json','acceptance.json']):
  path=root/name;size=path.stat().st_size;sha=digest(path);key=a.prefix+'/'+name
  if name in ('disk.qcow2','disk.raw','runtime.yaml'):
   expected=entry['disk' if name.startswith('disk.') else 'runtime_yaml']
   assert (sha,size)==(expected['sha256'],expected['size_bytes']),'local copy differs from accepted artifact'
  if name.startswith('disk.'):assert sha==accept['base_sha256_unchanged']
  assert size<=5*1024**3,'artifact requires existing multipart placement beyond single PUT bound'
  try:
   stat=store.stat(key)
  except exchange.ExchangeError as e:
   if str(e)!='exchange_s3_http_404':raise
   with path.open('rb') as body:
    request=urllib.request.Request(store.url('PUT',key),data=body,method='PUT',headers={'Content-Length':str(size),'If-None-Match':'*'})
    try:
     with urllib.request.build_opener(exchange.NoRedirect).open(request,timeout=30) as response:response.read(1024)
    except urllib.error.HTTPError as e:raise exchange.ExchangeError('artifact_put_http_'+str(e.code)) from None
    except Exception:raise exchange.ExchangeError('artifact_put_outcome_unknown; verify exact key before retry') from None
  store.verify(key,size,sha)
  receipt['objects'].append({'key':key,'size_bytes':size,'sha256':sha})
  print(json.dumps({'stage':'s3_object_verified','key':key,'size_bytes':size,'sha256':sha}),flush=True)
 (root/'publish-receipt.json').write_text(json.dumps(receipt,indent=2)+'\n')

if __name__=='__main__':main()
