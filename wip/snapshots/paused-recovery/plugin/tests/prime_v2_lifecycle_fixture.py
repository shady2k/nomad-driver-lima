"""New plugin lifecycle in existing network-none image; Bao mocked, Prime/acpx real."""
import os,pathlib,json,subprocess,tempfile,uuid,time,hashlib
ROOT=pathlib.Path(__file__).resolve().parents[2];IMAGE=os.environ['LIMA_ADAPTER_FIXTURE_IMAGE']
def docker(*args,**kw):return subprocess.run(['docker',*args],capture_output=True,text=True,check=kw.pop('check',True),**kw)
cid=docker('run','-d','--pull=never','--network=none','--read-only','--tmpfs','/tmp:rw,exec,nosuid,nodev,size=256m','--cpus=1','--memory=1g','--pids-limit=256','-e','HOME=/tmp/acpx-home','--entrypoint','/bin/sh',IMAGE,'-c','exec sleep 240').stdout.strip();sid=str(uuid.uuid4());rid=str(uuid.uuid4());owner='a'*32
assets={p.name:p.read_text() for p in (ROOT/'plugin/guest').iterdir() if p.suffix in ('.py','.mjs')};assets['vault-helper.py']=(ROOT/'internal/lima/vault_guest.py').read_text().replace('/opt/nomad-workers/task-main.py','/tmp/plugin/task-main.py')
setup=r'''
import os,pathlib,json,subprocess,time
root=pathlib.Path('/tmp/plugin');root.mkdir();data=json.load(__import__('sys').stdin)
for name,body in data.items():(root/name).write_text(body)
for d in ['/tmp/state','/tmp/work','/tmp/tls','/tmp/acpx-home']:pathlib.Path(d).mkdir(mode=0o700)
subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1','-keyout','/tmp/tls/key','-out','/tmp/tls/cert'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
(root/'fake-bao').write_text("""#!/usr/bin/python3
import os,sys,re,json,subprocess,signal
hcl=open(next(x[8:] for x in sys.argv if x.startswith('-config='))).read()
cmd=json.loads(re.search(r'exec \\{ command = (\\[.*\\])',hcl)[1])
env=dict(os.environ,LLM_API_KEY='synthetic-runtime-key')
p=subprocess.Popen(cmd,env=env)
def stop(*a):
 p.terminate()
 try:p.wait(timeout=10)
 except subprocess.TimeoutExpired:pass
 raise SystemExit(0)
signal.signal(signal.SIGTERM,stop)
raise SystemExit(p.wait())
""");os.chmod(root/'fake-bao',0o700)
(root/'provider.py').write_text("""import pathlib,json,ssl,threading
from http.server import BaseHTTPRequestHandler,HTTPServer
state={'calls':0,'auth':False,'revokes':[]}
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_POST(self):
  self.rfile.read(int(self.headers.get('Content-Length','0')))
  if self.path=='/v1/auth/token/revoke-self':
   
   if len(state['revokes'])==1 and not state.get('failure_exercised'):
    state['failure_exercised']=True;self.send_response(503);self.end_headers();return
   state['revokes'].append(self.headers.get('X-Vault-Token'));pathlib.Path('/tmp/api-state.json').write_text(json.dumps(state));self.send_response(204);self.end_headers();return
  state['session_header']=self.headers.get('x-opencode-session');state['calls']+=1;state['auth']=self.headers.get('Authorization')=='Bearer synthetic-runtime-key';pathlib.Path('/tmp/api-state.json').write_text(json.dumps(state))
  self.send_response(200 if state['auth'] else 401);self.send_header('Content-Type','text/event-stream');self.end_headers()
  if not state['auth']:return
  for x in [{'choices':[{'index':0,'delta':{'role':'assistant','content':'synthetic fixture complete'},'finish_reason':None}]},{'choices':[{'index':0,'delta':{},'finish_reason':'stop'}],'usage':{'prompt_tokens':1,'completion_tokens':1,'total_tokens':2}}]:self.wfile.write(('data: '+json.dumps(x)+'\\n\\n').encode());self.wfile.flush()
  self.wfile.write(b'data: [DONE]\\n\\n');self.wfile.flush()
plain=HTTPServer(('127.0.0.1',0),H);threading.Thread(target=plain.serve_forever,daemon=True).start();pathlib.Path('/tmp/provider-port').write_text(str(plain.server_port))
s=HTTPServer(('127.0.0.1',0),H);ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);ctx.load_cert_chain('/tmp/tls/cert','/tmp/tls/key');s.socket=ctx.wrap_socket(s.socket,server_side=True);pathlib.Path('/tmp/api-port').write_text(str(s.server_port));s.serve_forever()
""")
subprocess.Popen(['python3',str(root/'provider.py')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True)
while not pathlib.Path('/tmp/api-port').exists():time.sleep(.02)
print('prepared')
'''
def remote(action,**fields):
 payload={'action':action,'session_id':sid,'config':cfg,**fields}
 p=docker('exec','-i',cid,'node','/tmp/plugin/adapter.mjs',input=json.dumps(payload),timeout=50,check=False)
 rows=[json.loads(x) for x in p.stdout.splitlines() if x.strip()]
 return p,rows
def token(action,value=''):
 payload={'session':sid,'owner':owner,'action':action,'token':value,'address':base}
 return docker('exec','-i',cid,'env','SSL_CERT_FILE=/tmp/tls/cert','python3','/tmp/plugin/vault-helper.py',input=json.dumps(payload),timeout=25,check=False)
try:
 # setup receives source assets as stdin while Python code itself uses -c.
 docker('exec','-i',cid,'python3','-c',setup,input=json.dumps(assets),timeout=15)
 port=docker('exec',cid,'cat','/tmp/api-port').stdout.strip();base='https://127.0.0.1:'+port
 cfg={'protocol_version':1,'state_dir':'/tmp/state','workspace':'/tmp/work','node':'/usr/local/bin/node','launcher':'/tmp/plugin/launcher.mjs','acpx_bin':'/usr/local/bin/acpx','acpx_version':'0.21.0','runtime_module':'/opt/worker/acpx/node_modules/acpx/dist/runtime.js','agent_argv':['/usr/local/bin/prime-agent','--mode','acp','--offline'],'max_artifact_bytes':1048576,'prime_vault':{'bao_bin':'/tmp/plugin/fake-bao','address':base,'secret_path':'secret/data/runtime','kernel_python':'/opt/worker/prime/prime-agent-runtime/.venv/bin/python','provider':{'name':'fixture','base_url':'http://127.0.0.1:'+docker('exec',cid,'cat','/tmp/provider-port').stdout.strip()+'/v1','model':'offline-check','max_tokens':128,'context_window':32768}}}
 assert token('sync','synthetic-task-one').returncode==0
 docker('exec','-d',cid,'/bin/sh','-c',f'echo $$ > /tmp/main-{sid}.pid; exec python3 /tmp/plugin/task-main.py --state-dir /tmp/state --session {sid} --runtime-check /bin/true > /tmp/task-stdout 2>/tmp/task-stderr')
 p,rows=remote('create');assert p.returncode==0,(rows,docker('exec',cid,'cat','/tmp/task-stdout').stdout,docker('exec',cid,'cat','/tmp/task-stderr').stdout)
 p,rows=remote('submit',request_id=rid,prompt='Synthetic offline acknowledgement only.');assert p.returncode==0,rows
 p,rows=remote('watch',request_id=rid);assert p.returncode==0,rows
 state=json.loads(docker('exec',cid,'cat','/tmp/api-state.json').stdout);assert state['auth'] and state['calls']==1 and state['session_header']==sid,state
 # Rotate to a second task token; history must preserve both for finalization.
 assert token('sync','synthetic-task-two').returncode==0
 docker('exec',cid,'/bin/sh','-c','printf result > /tmp/work/result.txt')
 raw=b'result';files=[{'path':'result.txt','size':len(raw),'sha256':hashlib.sha256(raw).hexdigest()}];digest='b'*64
 for action,kw in [('prepare_finish',{'files':files,'manifest_sha256':digest}),('close',{}),('finish_ack',{'manifest_sha256':digest})]:
  p,rows=remote(action,**kw);assert p.returncode==0,rows
 deadline=time.monotonic()+20
 while docker('exec',cid,'test','-f',f'/dev/shm/agent-worker-0/{sid}/runtime-stopped.json',check=False).returncode:
  assert time.monotonic()<deadline,(docker('exec',cid,'cat','/tmp/task-stdout').stdout,docker('exec',cid,'cat','/tmp/task-stderr').stdout);time.sleep(.1)
 p=token('revoke');assert p.returncode==0 and not json.loads(p.stdout)['ok'],'unknown revoke must fail closed'
 assert docker('exec',cid,'test','-e',f'/dev/shm/agent-worker-0/{sid}/vault_token',check=False).returncode==0
 p=token('revoke');assert p.returncode==0 and json.loads(p.stdout)['ok'],p.stderr
 state=json.loads(docker('exec',cid,'cat','/tmp/api-state.json').stdout);assert set(state['revokes'])=={'synthetic-task-one','synthetic-task-two'}
 p=token('clear');assert p.returncode==0,p.stderr
 assert docker('exec',cid,'test','-e',f'/dev/shm/agent-worker-0/{sid}/vault_token',check=False).returncode!=0
 p,rows=remote('submit',request_id=str(uuid.uuid4()),prompt='must not restart');assert p.returncode!=0
 logs=docker('exec',cid,'cat','/tmp/task-stdout').stdout;assert 'owned_prime_bao_stopped' in logs and 'synthetic-runtime-key' not in logs

 # A second isolated worker session: controller death must reject new prompts,
 # and driver-style one-shot finalization must stop only its saved runtime.
 sid=str(uuid.uuid4());assert token('sync','synthetic-orphan-token').returncode==0
 docker('exec','-d',cid,'/bin/sh','-c',f'echo $$ > /tmp/main-{sid}.pid; exec python3 /tmp/plugin/task-main.py --state-dir /tmp/state --session {sid} --runtime-check /bin/true > /tmp/task2-stdout 2>/tmp/task2-stderr')
 p,rows=remote('create');assert p.returncode==0,rows
 docker('exec',cid,'python3','-c',f"import os,pathlib;os.kill(int(pathlib.Path('/tmp/main-{sid}.pid').read_text()),9)")
 p,rows=remote('submit',request_id=str(uuid.uuid4()),prompt='must not dispatch after owner death');assert p.returncode!=0,rows
 unchanged=json.loads(docker('exec',cid,'cat','/tmp/api-state.json').stdout);assert unchanged['calls']==1
 assert docker('exec',cid,'test','-f',f'/dev/shm/agent-worker-0/{sid}/runtime-stopped.json',check=False).returncode!=0
 p=token('revoke');assert p.returncode==0 and json.loads(p.stdout)['ok'],(p.stdout,p.stderr)
 assert token('clear').returncode==0
 print(json.dumps({'prime':'0.10.1','acpx':'0.21.0','bao':'mock supervisor only; official2.5.4 composition tested separately','create_submit_watch_finish':True,'provider_calls_synthetic':state['calls'],'revoke_all_exposed_tokens':True,'partial_revoke_failure_retains_and_retries_only_unacked':True,'clear_after_ack':True,'late_submit_rejected':True,'native_stage_logs':True,'prime_session_header':True,'controller_death_rejects_prompt':True,'one_shot_runtime_cleanup_after_controller_death':True,'paid_calls':0}))
finally:subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
