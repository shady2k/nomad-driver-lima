"""Reconcile only stock Vault config in existing Nomad server ConfigMap, then restart."""
import datetime,hashlib,json,pathlib,subprocess,sys
spec=json.loads(pathlib.Path(sys.argv[1]).read_text());mode=sys.argv[2];assert mode in ('check','apply')
k=spec['kubectl'];ns=spec['namespace'];cm=spec['configmap'];sts=spec['statefulset'];pod=spec['pod'];key=spec['config_key']
def run(args,data=None):
 r=subprocess.run([k,'-n',ns]+args,input=data,text=True,capture_output=True,timeout=180)
 if r.returncode:raise RuntimeError('kubectl '+args[0]+' failed: '+r.stderr[-1500:])
 return r.stdout
def get(kind,name):return json.loads(run(['get',kind,name,'-o','json']))
x=get('configmap',cm);old=x['data'][key];conf=json.loads(old)
vault=spec['vault_config']
assert isinstance(vault,list)and len(vault)==1 and vault[0].get('enabled')is True
from urllib.parse import urlsplit
u=urlsplit(vault[0]['address']);assert u.scheme=='https'and u.hostname and not u.username and not u.password and not u.query and not u.fragment
assert 'vault' not in conf or conf['vault']==vault,'Existing Vault drift requires review'
changed=conf.get('vault')!=vault
if changed and mode=='apply':
 assert spec['restart_approved'] is True
 root=pathlib.Path(spec['backup_dir']);root.mkdir(parents=True,exist_ok=True,mode=0o700)
 backup=root/'server-config.before.json'
 if not backup.exists():backup.write_text(old);backup.chmod(0o600)
 conf['vault']=vault;new=json.dumps(conf,indent=2)+'\n'
 # Validate with the actual running server binary and its mounted TLS paths.
 run(['exec','-i',pod,'--','sh','-c','umask 077; cat > /tmp/agent-vault-candidate.json; nomad config validate /tmp/agent-vault-candidate.json'],new)
 current=get('configmap',cm);assert current['metadata']['resourceVersion']==x['metadata']['resourceVersion'],'Concurrent ConfigMap change'
 patch={'metadata':{'resourceVersion':x['metadata']['resourceVersion']},'data':{key:new}}
 run(['patch','configmap',cm,'--type=merge','-p',json.dumps(patch)])
 assert json.loads(get('configmap',cm)['data'][key])==conf
 run(['rollout','restart','statefulset/'+sts])
 run(['rollout','status','statefulset/'+sts,'--timeout=120s'])
 p=get('pod',pod);assert any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions'])
 mounted=run(['exec',pod,'--','cat','/nomad/config/server.json']);assert json.loads(mounted)==conf
result={'mode':mode,'changed':changed,'server':ns+'/'+sts,'configmap':cm,'vault_config_readback':conf.get('vault')==vault,'restarted':changed and mode=='apply','client_restarted':False,'mac_jobs_modified':False}
print(json.dumps(result))
