"""Guarded Linux-only client Vault activation; preserves the approved keyless VM disk."""
import json,pathlib,subprocess,sys,re
s=json.loads(pathlib.Path(sys.argv[1]).read_text());mode=sys.argv[2]
assert mode in ['check','apply'];assert re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',s['vm'])
ssh=['ssh','-i',s['ssh_key'],'-o','IdentitiesOnly=yes','-o','ForwardAgent=no','-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o','ConnectTimeout=8']
def remote(host,cmd,code,timeout=120):
 host_options=['-o','HostKeyAlgorithms=ssh-ed25519'] if host==s['linux'] else []
 r=subprocess.run(ssh+host_options+[host,cmd],input=code,text=True,capture_output=True,timeout=timeout)
 if r.returncode:raise RuntimeError('Remote guarded action failed: '+r.stderr[-1200:])
 return r.stdout
def guard():
 code='''import json,urllib.request
v=json.load(open(settings['operator_bootstrap_file']))
r=urllib.request.Request(settings['operator_nomad_address'].rstrip('/')+'/v1/node/'+settings['node_id']+'/allocations',headers={'X-Nomad-Token':v['SecretID']})
with urllib.request.urlopen(r,timeout=15) as f:a=json.load(f)
assert not [x for x in a if x['ClientStatus'] not in ['complete','failed','lost']], 'Linux active allocations; refuse restart'
print('empty')
'''
 code='settings='+repr({k:s[k]for k in ('operator_bootstrap_file','operator_nomad_address','node_id')})+'\n'+code
 assert remote(s['operator'],'python3 -',code).strip()=='empty'
code='''import json,pathlib,subprocess,time
mode=MODE
activation=ACTIVATION
home=settings['runtime_home'];name=settings['vm']
def run(args):return subprocess.run(args,text=True,capture_output=True,check=True,timeout=100).stdout
base=['sudo','-u',settings['runtime_user'],'env','LIMA_HOME='+home,settings['limactl']]
rows=[json.loads(x) for x in run(base+['list','--json']).splitlines() if x.strip()]
running=[x for x in rows if x['status']=='Running'];assert all(x['name']==name for x in running),'Other isolated VM running'
vm=next(x for x in rows if x['name']==name);assert vm['dir']==home+'/'+name
p=pathlib.Path(settings['client_config']);old=p.read_text();conf=json.loads(old)
vault=settings['vault_config']
assert 'vault' not in conf or conf['vault']==vault,'Unexpected existing Vault settings'
changed=conf.get('vault')!=vault
activation_receipt=(pathlib.Path(settings['backup_dir'])/'driver-activation.json')
if activation:
 import hashlib
 assert hashlib.sha256(pathlib.Path(activation['binary']).read_bytes()).hexdigest()==activation['sha256'],'Driver binary drift'
 changed=changed or not activation_receipt.exists() or json.loads(activation_receipt.read_text())!=activation
if mode=='stop':
 if vm['status']=='Running':run(base+['stop',name])
 rows=[json.loads(x) for x in run(base+['list','--json']).splitlines() if x.strip()]
 assert next(x for x in rows if x['name']==name)['status']=='Stopped'
 assert pathlib.Path(home+'/'+name+'/disk').exists(),'Disk must be retained'
 print(json.dumps({'vm_stopped':True,'disk_retained':True}));raise SystemExit
if mode=='apply' and changed:
 # All qemu/logmon must be absent immediately before changing the service.
 live=subprocess.run(['pgrep','-f','qemu-system|nomad.*logmon'],capture_output=True,text=True)
 assert live.returncode==1,'Native VM/logmon activity; refuse restart'
 root=pathlib.Path(settings['backup_dir']);root.mkdir(mode=0o700,exist_ok=True)
 backup=root/'client.before.json'
 if not backup.exists():backup.write_text(old);backup.chmod(0o600)
 conf['vault']=vault;candidate=root/'client.candidate.json';candidate.write_text(json.dumps(conf,indent=2)+'\\n');candidate.chmod(0o600)
 run([settings['nomad_binary'],'config','validate',str(candidate)])
 assert p.read_text()==old,'Concurrent config change'
 p.write_text(candidate.read_text())
 run(['systemctl','restart',settings['client_service']]);run(['systemctl','is-active',settings['client_service']])
 if activation:activation_receipt.write_text(json.dumps(activation)+'\\n');activation_receipt.chmod(0o600)
assert json.loads(p.read_text()).get('vault')==vault if mode=='apply' else True
print(json.dumps({'changed':changed,'mode':mode,'client_restarted':changed and mode=='apply','disk_retained':True}))
'''
code='settings='+repr({k:s[k]for k in ('runtime_home','vm','runtime_user','limactl','client_config','vault_config','backup_dir','nomad_binary','client_service')})+'\n'+code
code=code.replace('ACTIVATION',repr(s.get('activation')))
if mode=='apply':
 assert s['restart_approved'] is True
 guard();stop=json.loads(remote(s['linux'],'sudo -n python3 -',code.replace('MODE',repr('stop'))));guard()
result=json.loads(remote(s['linux'],'sudo -n python3 -',code.replace('MODE',repr(mode))))
print(json.dumps(result))
