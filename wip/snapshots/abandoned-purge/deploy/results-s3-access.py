"""Reconcile one prefix-scoped result identity, keeping credentials only in Bao/memory."""
import datetime,hashlib,hmac,json,os,pathlib,secrets,string,subprocess,sys,urllib.error,urllib.parse,urllib.request,uuid
spec=json.loads(pathlib.Path(sys.argv[1]).read_text());assert spec['approved'] is True
import re
name=spec['identity'];policy_name=spec['policy'];bucket=spec['bucket'];bao_path=spec['bao_path']
assert all(isinstance(v,str)and re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]{0,127}',v)for v in (name,policy_name,bucket))
assert re.fullmatch('[A-Za-z0-9_-]+/data/[A-Za-z0-9_/-]+',bao_path)
k=spec['kubectl'];base=[k,'-n',spec['storage_namespace'],'exec','-i',spec['storage_pod'],'-c',spec['storage_container'],'--']
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*a,**kw):raise RuntimeError('redirect refused')
opener=urllib.request.build_opener(NoRedirect)
def shell(command,missing=False):
 p=subprocess.run(base+['/usr/bin/weed','shell','-master=127.0.0.1:9333','-filer=127.0.0.1:8888'],input=command+'\n',text=True,capture_output=True,timeout=40)
 if p.returncode:
  if missing and any(x in (p.stdout+p.stderr).lower()for x in ['not found','notfound']):return None
  raise RuntimeError('official IAM command failed; raw output suppressed')
 return json.loads(p.stdout) if p.stdout.strip() else None
def getuser(who):return shell('s3.user.show -name '+who,True)
def safeuser(d):
 return {k:d.get(k)for k in ['name','status','source','policies','actions']} if d else None
version=subprocess.run(base+['/usr/bin/weed','version'],capture_output=True,text=True,timeout=15)
assert version.returncode==0 and '4.51 ffa87c25f' in version.stdout,'Pinned running version differs'
existing={n:safeuser(getuser(n))for n in spec['preserve_identities']};assert all(existing.values())
resources=['arn:aws:s3:::'+bucket+'/jobs/worker-*/sessions/*/requests/*/'+part+'/*'for part in ['objects','manifests','commits']]
policy={'Version':'2012-10-17','Statement':[{'Sid':'DeclaredResultObjectsOnly','Effect':'Allow','Action':['s3:GetObject','s3:PutObject'],'Resource':resources}]}
oldpolicy=shell('s3.policy -get -name '+policy_name,True);assert oldpolicy in (None,policy),'Existing policy drift'
user=getuser(name);assert not user or (not user.get('actions') and user.get('policies',[]) in ([],[policy_name])),'Existing identity has additional access'
# Existing admin credential is consumed locally, never installed on caller or guest.
token_file=pathlib.Path(spec['admin_token_file']);assert not token_file.is_symlink() and token_file.stat().st_mode&0o777==0o600
token=token_file.read_text().strip()
def bao(path,method='GET',value=None,missing=False):
 r=urllib.request.Request(spec['bao_address']+'/v1/'+path,method=method,headers={'X-Vault-Token':token,'Content-Type':'application/json'},data=None if value is None else json.dumps(value).encode())
 try:
  with opener.open(r,timeout=15)as f:b=f.read();return json.loads(b)if b else None
 except urllib.error.HTTPError as e:
  if missing and e.code==404:return None
  raise RuntimeError('OpenBao operation HTTP '+str(e.code))from None
record=bao(bao_path,missing=True);changes=[]
if record is None:
 assert user is None,'Existing IAM identity has no approved Bao binding'
 alphabet=string.ascii_uppercase+string.digits
 pair={'access_key_id':''.join(secrets.choice(alphabet)for _ in range(20)),'secret_access_key':secrets.token_hex(20)}
 bao(bao_path,'POST',{'options':{'cas':0},'data':pair});changes.append('bao_results_record')
 record=bao(bao_path)
pair=record['data']['data'];assert set(pair)=={'access_key_id','secret_access_key'}
ak,sk=pair['access_key_id'],pair['secret_access_key'];assert all(isinstance(x,str)and x and all(c in string.ascii_letters+string.digits for c in x)for x in [ak,sk])
if oldpolicy is None:
 raw=json.dumps(policy).encode();p=subprocess.run(base+['tee','/tmp/agent-worker-results-policy.json'],input=raw,capture_output=True,timeout=15);assert p.returncode==0
 shell('s3.policy -put -name '+policy_name+' -file /tmp/agent-worker-results-policy.json');changes.append('scoped_policy')
if user is None:
 # Secrets travel as stdin of the existing exec channel, never as process argv.
 shell('s3.user.create -name '+name+' -access_key '+ak+' -secret_key '+sk);changes.append('results_identity')
user=getuser(name);assert len(user.get('credentials',[]))==1 and user['credentials'][0]['access_key']==ak,'Credential binding differs'
if user.get('policies',[])!=[policy_name]:shell('s3.policy.attach -policy '+policy_name+' -user '+name);changes.append('policy_binding')
user=getuser(name);assert user['policies']==[policy_name] and not user.get('actions') and user['status']=='enabled'
assert shell('s3.policy -get -name '+policy_name)==policy
assert {n:safeuser(getuser(n))for n in existing}==existing,'Existing identities changed'
# Signed requests use the new scoped key only. No URLs/signatures are logged.
def request(method,key='',body=None,query=None):
 now=datetime.datetime.now(datetime.timezone.utc);stamp=now.strftime('%Y%m%dT%H%M%SZ');day=stamp[:8];region='us-east-1';scope=day+'/'+region+'/s3/aws4_request'
 host=urllib.parse.urlsplit(spec['endpoint']).netloc;uri='/'+bucket+'/'+urllib.parse.quote(key,safe='/~')
 q={'X-Amz-Algorithm':'AWS4-HMAC-SHA256','X-Amz-Credential':ak+'/'+scope,'X-Amz-Date':stamp,'X-Amz-Expires':'60','X-Amz-SignedHeaders':'host'};q.update(query or {})
 q=urllib.parse.urlencode(sorted(q.items()),quote_via=urllib.parse.quote,safe='~')
 canonical='\n'.join([method,uri,q,'host:'+host+'\n','host','UNSIGNED-PAYLOAD']);tosign='AWS4-HMAC-SHA256\n'+stamp+'\n'+scope+'\n'+hashlib.sha256(canonical.encode()).hexdigest()
 def sig(key,value):return hmac.new(key,value.encode(),hashlib.sha256).digest()
 signing=sig(('AWS4'+sk).encode(),day)
 for v in [region,'s3','aws4_request']:signing=sig(signing,v)
 url=spec['endpoint']+uri+'?'+q+'&X-Amz-Signature='+hmac.new(signing,tosign.encode(),hashlib.sha256).hexdigest()
 r=urllib.request.Request(url,method=method,data=body)
 if body is not None:r.add_header('If-None-Match','*')
 try:
  with opener.open(r,timeout=20)as f:return f.status,f.read(1048576)
 except urllib.error.HTTPError as e:return e.code,b''
root=pathlib.Path(spec['evidence_dir']);root.mkdir(parents=True,exist_ok=True,mode=0o700)
testfile=root/'test-identity.json'
if testfile.exists():ids=json.loads(testfile.read_text())
else:
 ids={x:str(uuid.uuid4())for x in ['job','session','request','nonce']};testfile.write_text(json.dumps(ids));testfile.chmod(0o600)
prefix='jobs/worker-'+ids['job']+'/sessions/'+ids['session']+'/requests/'+ids['request']+'/'
# A rerun verifies the same immutable safe fixture; it never replays a model request.
raw=('nomad-workers scoped result acceptance '+ids['nonce']+'\n').encode();sha=hashlib.sha256(raw).hexdigest();object_key=prefix+'objects/'+sha
code,b=request('GET',object_key)
if code==404:
 code,_=request('PUT',object_key,raw);assert code==200,('result_put',code)
else:assert code==200 and b==raw,('prior_result_get',code)
code,b=request('GET',object_key);assert code==200 and b==raw
item={'path':'acceptance.txt','size':len(raw),'sha256':sha,'executable':False}
doc={'version':1,'scope':'declared-terminal-results','job':'worker-'+ids['job'],'session_id':ids['session'],'request_id':ids['request'],'files':[item],'storage_kind':'s3','replicated_backup_verified':False}
canonical=lambda d:(json.dumps(d,sort_keys=True,separators=(',',':'))+'\n').encode()
manifest=canonical(doc);msha=hashlib.sha256(manifest).hexdigest();manifest_key=prefix+'manifests/'+msha+'.json';commit=canonical({'manifest_sha256':msha,'manifest_key':manifest_key})
for key,content in [(manifest_key,manifest),(prefix+'commits/'+msha+'.json',commit)]:
 code,b=request('GET',key)
 if code==404:assert request('PUT',key,content)[0]==200
 else:assert code==200 and b==content
 assert request('GET',key)==(200,content)
negative={}
for label,key in [('outside_job','jobs/other-'+ids['nonce']+'/objects/probe'),('outside_result_part',prefix+'workspace/probe-'+ids['nonce']),('base_artifact','environments/agent-base/denied-'+ids['nonce'])]:
 for method in ['GET','PUT']:
  code,_=request(method,key,b'nonsecret scope probe'if method=='PUT'else None);negative[label+'_'+method]=code;assert code==403,(label,method,code)
# DELETE targets a never-created unique object, so even an unexpected allow cannot delete data.
code,_=request('DELETE',prefix+'objects/never-created-'+ids['nonce']);negative['delete_absent']=code;assert code==403
code,_=request('GET',query={'list-type':'2'});negative['list_bucket']=code;assert code==403
receipt={'schema_version':1,'identity':name,'policy':policy_name,'changes':changes,'changed':bool(changes),'policy_document':policy,'bao_path':bao_path,'existing_identities_unchanged':True,'secret_values_emitted':False,'model_calls':0,'test':{'prefix':prefix,'file_sha256':sha,'bytes':len(raw),'manifest_sha256':msha,'readback_verified':True,'negative_http':negative},'time':datetime.datetime.now(datetime.timezone.utc).isoformat()}
p=root/'receipt.json';p.write_text(json.dumps(receipt,indent=2)+'\n');p.chmod(0o600)
print(json.dumps(receipt))
