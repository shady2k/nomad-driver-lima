"""Install only secret-free storage references using the caller's existing Bao auth."""
import hashlib,importlib.util,json,os,pathlib,stat,sys,uuid
s=json.loads(sys.argv[1]);home=pathlib.Path(s['caller_home']);root=home/'.config/nomad-workers';assert root.is_dir()and not root.is_symlink()
ref=root/'exchange-results.json';client=root/'catalog-client.json'
expected=s['reference']
assert set(expected)=={'endpoint','bucket','region','bao_address','bao_path','token_file'}
assert os.getuid()==home.stat().st_uid and s['reference']==expected
changes=[]
def write(p,d):
 assert not p.is_symlink();tmp=p.with_suffix(p.suffix+'.tmp');fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w')as f:json.dump(d,f,indent=2);f.write('\n');f.flush();os.fsync(f.fileno())
 os.replace(tmp,p)
if ref.exists():assert json.loads(ref.read_text())==expected and stat.S_IMODE(ref.stat().st_mode)==0o600
else:write(ref,expected);changes.append('results_reference')
assert client.is_file()and not client.is_symlink()and stat.S_IMODE(client.stat().st_mode)==0o600
c=json.loads(client.read_text());assert c['namespace']=='agent-workers'
settings={'backend':'s3','reader_config':str(ref),'writer_config':str(ref)}
if c.get('storage')!=settings:
 old=c.get('storage')
 assert old is None or (set(old)<= {'backend','reader_config','writer_config'} and old.get('backend')=='s3' and old.get('reader_config')==s.get('legacy_reader_config') and not old.get('writer_config')),'Existing storage binding differs'
 backup=root/'catalog-client.before-results.json'
 if backup.exists():assert json.loads(backup.read_text())==c
 else:write(backup,c)
 c['storage']=settings;write(client,c);changes.append('catalog_results_binding')
# Use installed official caller adapter with existing local token; nothing is forwarded.
module=pathlib.Path(s['exchange_module']);assert module.is_file()and module.is_relative_to(home/'.local/share/claude-plugins/nomad-workers/releases')
load=importlib.util.spec_from_file_location('approved_exchange',module);ex=importlib.util.module_from_spec(load);load.loader.exec_module(ex)
store=ex.Store(ex.config(ref));raw=store.get(s['fixture_object']);assert hashlib.sha256(raw).hexdigest()==s['fixture_sha256']
proof=root/'results-access-proof.json'
if proof.exists():p=json.loads(proof.read_text());key=p['object'];data=p['content'].encode()
else:
 session=str(uuid.uuid4());request=str(uuid.uuid4());job='worker-'+str(uuid.uuid4());data=b'nomad-workers native caller result access acceptance\n';sha=hashlib.sha256(data).hexdigest();key=ex.identity(job,session,request)+'objects/'+sha
 p={'object':key,'content':data.decode(),'sha256':sha};write(proof,p)
try:stored=store.get(key)
except ex.ExchangeError as e:
 assert str(e)=='exchange_s3_http_404';store.put(key,data);stored=store.get(key)
assert stored==data
# Verify the installed caller's native immutable manifest/commit receipt protocol.
sys.path.insert(0,str(module.parent));import storage
parts=key.split('/');job,session,request=parts[1],parts[3],parts[5]
out=root/'results-access-fixture';out.mkdir(mode=0o700,exist_ok=True);f=out/'acceptance.txt'
if f.exists():assert f.read_bytes()==data
else:
 fd=os.open(f,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'wb')as stream:stream.write(data)
receipt_file=out/'receipt.json';attempt={'fixture':True,'model_calls':0,'diagnostics_required':False}
if receipt_file.exists():
 result=json.loads(receipt_file.read_text());storage.verify(settings,result['receipt'],result['backend_binding'],attempt_binding=attempt)
else:
 result=storage.commit(settings,job,session,request,[{'path':'acceptance.txt','size':len(data),'sha256':hashlib.sha256(data).hexdigest(),'executable':False,'local_file':'acceptance.txt'}],out,attempt);write(receipt_file,result)
print(json.dumps({'changed':bool(changes),'changes':changes,'existing_bao_auth_used_in_place':True,'new_tokens_or_acl_grants':False,'reference':str(ref),'client_storage':settings,'caller_put_get_sha_verified':True,'caller_object':key,'caller_object_sha256':hashlib.sha256(data).hexdigest(),'previous_fixture_sha_verified':True,'native_commit_receipt_verified':True,'native_manifest_sha256':result['receipt']['manifest_sha256'],'model_calls':0,'credential_values_emitted':False}))
