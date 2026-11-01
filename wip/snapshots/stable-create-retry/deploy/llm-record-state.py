"""CAS-create only LLM_API_KEY within the same OpenBao; never print/store values."""
import json,os,pathlib,stat,sys,urllib.error,urllib.request,re
spec_file,mode=sys.argv[1:];s=json.loads(pathlib.Path(spec_file).read_text());assert mode in ('check','apply')
assert s.get('record_paths_approved')is True and all(re.fullmatch('[A-Za-z0-9_-]+/data/[A-Za-z0-9_/-]+',s[k])for k in ('source','destination'))
assert s['source']!=s['destination']and s['source'].split('/')[0]==s['destination'].split('/')[0]
p=pathlib.Path(s['admin_token_file']);st=p.stat();assert st.st_uid==os.getuid() and stat.S_IMODE(st.st_mode)==0o600 and not p.is_symlink();token=p.read_text().strip()
def call(path,method='GET',value=None,missing=False):
 req=urllib.request.Request(s['address']+'/v1/'+path,headers={'X-Vault-Token':token,'Content-Type':'application/json'},method=method,data=None if value is None else json.dumps(value).encode())
 try:
  with urllib.request.urlopen(req,timeout=15) as f:
   raw=f.read(131073);assert len(raw)<=131072;return json.loads(raw) if raw else None
 except urllib.error.HTTPError as e:
  if missing and e.code==404:return None
  raise RuntimeError('OpenBao record operation HTTP '+str(e.code)) from None
source_meta=call(s['source'].replace('/data/','/metadata/',1));dest_meta=call(s['destination'].replace('/data/','/metadata/',1),missing=True)
changed=dest_meta is None
if mode=='apply':
 source=call(s['source'])['data']['data'];key=source.get('LLM_API_KEY');assert isinstance(key,str) and key and len(key)<8192,'Expected approved LLM field missing'
 del source
 current=call(s['destination'],missing=True)
 if current is None:
  # CAS0 never overwrites an existing record, including concurrent creation.
  call(s['destination'],'POST',{'options':{'cas':0},'data':{'LLM_API_KEY':key}})
 else:
  data=current['data']['data'];assert set(data)=={'LLM_API_KEY'} and data['LLM_API_KEY']==key,'Destination differs; refuse overwrite';del data
 after=call(s['destination']);data=after['data']['data'];assert set(data)=={'LLM_API_KEY'} and data['LLM_API_KEY']==key,'Record readback mismatch';version=after['data']['metadata']['version'];del data,key,current,after
else:version=dest_meta['data']['current_version'] if dest_meta else None
result={'mode':mode,'changed':changed,'source':s['source'],'source_unchanged':True,'destination':s['destination'],'only_field':'LLM_API_KEY','destination_version':version,'unrelated_fields_copied':False,'secret_values_logged_or_stored_locally':False,'readback_verified':mode=='apply','vm_credential_delivery':False}
if mode=='apply':
 p=pathlib.Path(s['receipt_file']);p.parent.mkdir(mode=0o700,parents=True,exist_ok=True);p.write_text(json.dumps(result,indent=2)+'\n');p.chmod(0o600)
print(json.dumps(result))
