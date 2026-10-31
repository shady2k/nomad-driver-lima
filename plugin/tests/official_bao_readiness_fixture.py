"""Official Bao2.5.4 + Prime0.10.1 Linux readiness/cleanup regression.
Existing local image and SHA-verified official archive only; network none,
loopback synthetic API, no business secrets/model requests or host mounts.
"""
import pathlib,os,json,subprocess,sys,hashlib
repo=pathlib.Path(__file__).resolve().parents[2]
image=os.environ['LIMA_ADAPTER_FIXTURE_IMAGE']
archive=pathlib.Path(os.environ['BAO_FIXTURE_ARCHIVE'])
assert hashlib.sha256(archive.read_bytes()).hexdigest()=='7690853adccaf9bc413768e6bfed4d0ac5222b67f808ba71af0930189f22f94f'
def docker(*args,**kw):return subprocess.run(['docker',*args],capture_output=True,text=True,check=kw.pop('check',True),**kw)
cid=docker('run','-d','--pull=never','--network=none','--cpus=1','--memory=1g','--pids-limit=256','--tmpfs','/tmp:rw,exec,nosuid,nodev,size=512m','--entrypoint','/bin/sh',image,'-c','exec sleep 180').stdout.strip()
try:
 with archive.open('rb') as stream:subprocess.run(['docker','exec','-i',cid,'tar','-xz','-C','/tmp'],stdin=stream,check=True,timeout=30,stdout=subprocess.DEVNULL)
 docker('exec','-i',cid,'python3','-c',"import pathlib,sys;pathlib.Path('/tmp/task-main.py').write_text(sys.stdin.read())",input=(repo/'plugin/guest/task-main.py').read_text())
 code=r'''
import pathlib,os,subprocess,ssl,threading,json,time,importlib.util,uuid,signal,stat
from http.server import BaseHTTPRequestHandler,HTTPServer

assert '2.5.4' in subprocess.check_output(['/tmp/bao','version'],text=True)
subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-days','1','-subj','/CN=localhost','-addext','subjectAltName=IP:127.0.0.1','-keyout','/tmp/key','-out','/tmp/cert'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
with open('/etc/ssl/certs/ca-certificates.crt','ab') as f:f.write(pathlib.Path('/tmp/cert').read_bytes())
counts={'lookups':0,'secrets':0,'model_calls':0}
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_GET(self):
  if self.path.startswith('/v1/sys/internal/ui/mounts'):v={'data':{'type':'kv','path':'secret/','options':{'version':'2'}}}
  elif self.path.startswith('/v1/sys/health'):v={'initialized':True,'sealed':False,'version':'2.5.4'}
  elif self.path.startswith('/v1/auth/token/lookup-self'):counts['lookups']+=1;v={'data':{'id':'synthetic-task-token','ttl':1800,'renewable':True,'type':'service','policies':['fixture']}}
  elif self.path.startswith('/v1/secret/data/runtime'):counts['secrets']+=1;v={'data':{'data':{'LLM_API_KEY':'synthetic-key'}}}
  else:self.send_response(404);self.end_headers();return
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps(v).encode())
 def do_POST(self):
  self.rfile.read(int(self.headers.get('Content-Length','0')))
  if self.path=='/v1/auth/token/renew-self':v={'auth':{'client_token':'synthetic-task-token','lease_duration':1800,'renewable':True}}
  else:counts['model_calls']+=1;self.send_response(500);self.end_headers();return
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps(v).encode())
s=HTTPServer(('127.0.0.1',0),H);ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);ctx.load_cert_chain('/tmp/cert','/tmp/key');s.socket=ctx.wrap_socket(s.socket,server_side=True);threading.Thread(target=s.serve_forever,daemon=True).start()
spec=importlib.util.spec_from_file_location('task','/tmp/task-main.py');m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
sid=str(uuid.uuid4());root=m.token_root(sid);(root/'vault_token').write_text('synthetic-task-token');(root/'vault_token').chmod(0o600)
d=m.private('/tmp/state');work=m.private('/tmp/work')
c={'workspace':str(work),'agent_argv':['/usr/local/bin/prime-agent','--mode','acp','--offline'],'prime_vault':{'address':'https://127.0.0.1:'+str(s.server_port),'secret_path':'secret/data/runtime','bao_bin':'/tmp/bao','kernel_python':'/opt/worker/prime/prime-agent-runtime/.venv/bin/python','provider':{'name':'fixture','model':'offline-check','base_url':'http://127.0.0.1:9/v1','max_tokens':128,'context_window':32768}}}
start=time.monotonic();error=None
try:m.start_runtime(d,sid,{'config':c,'config_hash':'synthetic'})
except Exception as e:error=str(e)
owner=json.loads((root/'runtime-owner.json').read_text());p=owner['processes'][0];leader=[];other=[];matches=[]
for tid in pathlib.Path('/proc/'+str(p['pid'])+'/task').iterdir():
 try:children=(tid/'children').read_text().split()
 except FileNotFoundError:continue
 if tid.name==str(p['pid']):leader+=children
 else:other+=children
 for child in children:
  try:
   argv=pathlib.Path('/proc/'+child+'/cmdline').read_bytes().split(b'\0');exe=os.path.realpath('/proc/'+child+'/exe')
   if b'--mode' in argv and b'daemon' in argv:matches.append({'pid':int(child),'ticks':m.identity(int(child))['ticks'],'expected_executable':exe==os.path.realpath('/usr/local/bin/prime-agent')})
  except (FileNotFoundError,TypeError):pass
socket=root/'prime.sock'
assert error is None,'readiness failed'
assert counts['lookups']>=1 and counts['secrets']>=1 and counts['model_calls']==0
assert m.socket_owned_by(owner['processes'][1],socket),'socket not held by daemon'
assert not m.socket_owned_by(p,socket),'supervisor falsely owns daemon socket'
assert not m.socket_owned_by({'pid':owner['processes'][1]['pid'],'ticks':'reused'},socket)
all_owned=m.descendants(owner['processes']);m.stop_runtime(root)
assert not any(m.alive(x) for x in all_owned),'owned cleanup incomplete'
assert json.loads((root/'runtime-stopped.json').read_text())['stopped']
# An official Bao supervisor with a failing child must produce safe diagnostics.
second=str(uuid.uuid4());root2=m.token_root(second);(root2/'vault_token').write_text('synthetic-task-token');(root2/'vault_token').chmod(0o600)
d2=m.private('/tmp/failed-state');c2={**c,'agent_argv':['/bin/false']}
failure=None
try:m.start_runtime(d2,second,{'config':c2,'config_hash':'synthetic-failure'})
except RuntimeError as e:failure=str(e)
assert failure=='BAO_SUPERVISOR_DIED',failure
safe=json.loads((d2/'runtime-readiness-diagnostics.json').read_text())
assert safe['supervisor_exit_code'] is not None and safe['token']['private'] and not safe['socket']['present']
assert 'synthetic-key' not in json.dumps(safe) and 'synthetic-task-token' not in json.dumps(safe)
m.stop_runtime(root2)
print(json.dumps({'result_error':error,'elapsed_seconds':round(time.monotonic()-start,2),'readiness_success':True,'leader_children_count':len(leader),'other_thread_children_count':len(other),'matching_daemons':len(matches),'daemon_executable_matches':all(x['expected_executable'] for x in matches),'socket_owned_by_exact_daemon':True,'wrong_pid_or_supervisor_rejected':True,'owned_runtime_cleanup':True,'official_bao_failed_child_diagnostics':True,**counts}),flush=True)
s.shutdown()
'''
 p=docker('exec','-i',cid,'python3','-',input=code,timeout=60,check=False)
 # Readiness probe emits only bounded safe evidence, never supervisor stderr.
 for line in p.stdout.splitlines():
  try:
   obj=json.loads(line)
   if 'result_error' in obj:print(json.dumps(obj))
  except ValueError:pass
 if p.returncode:
  print(json.dumps({'probe_exit_code':p.returncode,'stderr_redacted':p.stderr.replace('synthetic-task-token','[redacted]').replace('synthetic-key','[redacted]')[-1600:],'exception_type':p.stderr.splitlines()[-1].split(':',1)[0] if p.stderr else None,'trace_locations':[x.strip() for x in p.stderr.splitlines() if x.lstrip().startswith('File ')]}));sys.exit(1)
finally:subprocess.run(['docker','rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
