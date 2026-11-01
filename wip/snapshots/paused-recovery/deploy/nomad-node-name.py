"""Change only approved client display name; preserve node data/auth identity."""
import json,pathlib,subprocess,sys,shlex
s=json.loads(pathlib.Path(sys.argv[1]).read_text());mode=sys.argv[2];assert mode in ['check','apply'];assert s['old_name']=='boxlite-linux-01' and s['new_name']=='agent-linux-01'
ssh=['ssh','-i',s['ssh_identity'],'-o','IdentitiesOnly=yes','-o','ForwardAgent=no','-o','StrictHostKeyChecking=yes','-o','BatchMode=yes','-o','ConnectTimeout=8']
def remote(host,command,program):
 extra=['-o','HostKeyAlgorithms=ssh-ed25519'] if host==s['linux_host'] else []
 r=subprocess.run(ssh+extra+[host,command],input=program,text=True,capture_output=True,timeout=90)
 if r.returncode:raise RuntimeError('Guarded node naming failed: '+r.stderr[-1000:])
 return r.stdout
probe='''import subprocess,json
r=subprocess.run([CLI,'operator','api','-X','GET','/v1/node/'+NODE+'/allocations'],capture_output=True,text=True,check=True)
assert not [x for x in json.loads(r.stdout) if x['ClientStatus'] not in ['complete','failed','lost']],'Linux active work; refuse restart'
print('empty')
'''.replace('CLI',repr(s['operator_cli'])).replace('NODE',repr(s['node']))
if mode=='apply':assert s['approved'] is True and remote(s['operator_host'],'python3 -',probe).strip()=='empty'
program=r'''import pathlib,json,subprocess,hashlib,os,tempfile
p=pathlib.Path(CONFIG);old=p.read_bytes();c=json.loads(old);assert c['name'] in [OLD,NEW]
changed=c['name']!=NEW
if changed and MODE=='apply':
 assert hashlib.sha256(old).hexdigest()==EXPECTED,'Concurrent/unreviewed configuration drift'
 assert subprocess.run(['pgrep','-f','qemu-system|nomad.*logmon'],capture_output=True).returncode==1,'Native VM/logmon active'
 root=pathlib.Path('/var/lib/nomad-lima/node-name-backup');root.mkdir(mode=0o700,exist_ok=True);backup=root/'client.before.json'
 if not backup.exists():backup.write_bytes(old);backup.chmod(0o600)
 original=json.loads(old);c['name']=NEW;candidate=root/'client.candidate.json';candidate.write_text(json.dumps(c,indent=2)+'\n');candidate.chmod(0o600)
 subprocess.run([NOMAD,'config','validate',str(candidate)],capture_output=True,text=True,check=True,timeout=20)
 assert p.read_bytes()==old
 fd,temp=tempfile.mkstemp(prefix='.node-name-',dir=p.parent)
 with os.fdopen(fd,'wb') as f:f.write(candidate.read_bytes());f.flush();os.fsync(f.fileno())
 os.replace(temp,p);directory=os.open(p.parent,os.O_RDONLY);os.fsync(directory);os.close(directory)
 assert {k:v for k,v in json.loads(p.read_text()).items() if k!='name'}=={k:v for k,v in original.items() if k!='name'}
 subprocess.run(['systemctl','restart',SERVICE],capture_output=True,text=True,check=True,timeout=60)
 subprocess.run(['systemctl','is-active',SERVICE],capture_output=True,text=True,check=True,timeout=10)
print(json.dumps({'changed':changed,'mode':MODE,'name':json.loads(p.read_text())['name'],'config_sha256':hashlib.sha256(p.read_bytes()).hexdigest(),'data_auth_paths_unchanged':True,'vm_or_disk_deleted':False}))
'''
for name,value in {'CONFIG':s['config'],'OLD':s['old_name'],'NEW':s['new_name'],'MODE':mode,'EXPECTED':s['expected_before_sha256'],'NOMAD':s['nomad'],'SERVICE':s['service']}.items():program=program.replace(name,repr(value))
print(remote(s['linux_host'],'sudo -n python3 -',program).strip())
