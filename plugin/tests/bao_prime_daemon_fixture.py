"""Synthetic composition fixture: upstream Bao -> Prime daemon; direct ACP over socket.
Docker is an existing test harness only, never the target guest architecture.
"""
import os,sys,json,subprocess,pathlib,tempfile,threading,time,queue,shutil
from http.server import BaseHTTPRequestHandler,HTTPServer
IMAGE=os.environ['LIMA_ADAPTER_FIXTURE_IMAGE'];BAO=shutil.which(os.environ.get('BAO_BIN','bao'));DOCKER=shutil.which('docker');assert BAO and DOCKER
def docker(*args,**kw):return subprocess.run([DOCKER,*args],capture_output=True,text=True,check=kw.pop("check",True),**kw)
host=docker('context','inspect','--format','{{.Endpoints.docker.Host}}').stdout.strip()
cid=docker('run','-d','--pull=never','--network=none','--read-only','--tmpfs','/tmp:rw,nosuid,nodev,size=256m','--cpus=1','--memory=1g','--pids-limit=128','--entrypoint','/bin/sh',IMAGE,'-c','exec sleep 180').stdout.strip()
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_GET(self):
  if self.path.startswith('/v1/sys/internal/ui/mounts'):v={'data':{'type':'kv','path':'secret/','options':{'version':'2'}}}
  elif self.path.startswith('/v1/sys/health'):v={'initialized':True,'sealed':False,'version':'2.5.4'}
  elif self.path.startswith('/v1/auth/token/lookup-self'):v={'data':{'id':'synthetic-task-token','ttl':3600,'renewable':True,'type':'service','policies':['synthetic']}}
  else:v={'data':{'data':{'LLM_API_KEY':'synthetic-key'}}}
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps(v).encode())
srv=HTTPServer(('127.0.0.1',0),H);threading.Thread(target=srv.serve_forever,daemon=True).start();supervisor=None
setup=r'''
import json,pathlib,subprocess,os
root=pathlib.Path('/tmp/probe');root.mkdir();(root/'config').mkdir();(root/'work').mkdir()
(root/'provider.py').write_text("""from http.server import BaseHTTPRequestHandler,HTTPServer
import json,pathlib
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_POST(self):
  self.rfile.read(int(self.headers['Content-Length']))
  good=self.headers.get('Authorization')=='Bearer synthetic-key'
  pathlib.Path('/tmp/probe/provider-auth.json').write_text(json.dumps({'authorization_correct':good}))
  self.send_response(200 if good else 401);self.send_header('Content-Type','text/event-stream');self.end_headers()
  if not good:return
  for v in [{'choices':[{'index':0,'delta':{'role':'assistant','content':'synthetic transport fixture complete'},'finish_reason':None}]},{'choices':[{'index':0,'delta':{},'finish_reason':'stop'}],'usage':{'prompt_tokens':1,'completion_tokens':1,'total_tokens':2}}]:
   self.wfile.write(('data: '+json.dumps(v)+'\\n\\n').encode());self.wfile.flush()
  self.wfile.write(b'data: [DONE]\\n\\n');self.wfile.flush()
s=HTTPServer(('127.0.0.1',0),H)
pathlib.Path('/tmp/probe/provider-port').write_text(str(s.server_port));s.serve_forever()
""")
p=subprocess.Popen(['python3',str(root/'provider.py')],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True)
import time
while not (root/'provider-port').exists():time.sleep(.02)
port=(root/'provider-port').read_text()
model={'id':'offline-check','name':'offline-check','api':'openai-completions','reasoning':False,'input':['text'],'cost':{'input':0,'output':0,'cacheRead':0,'cacheWrite':0},'contextWindow':32768,'maxTokens':128}
(root/'config/models.json').write_text(json.dumps({'providers':{'fixture':{'baseUrl':'http://127.0.0.1:'+port+'/v1','apiKey':'LLM_API_KEY','api':'openai-completions','models':[model]}}}))
print('prepared')
'''
try:
 docker('exec','-i',cid,'python3','-',input=setup,timeout=10)
 with tempfile.TemporaryDirectory() as tmp:
  d=pathlib.Path(tmp);(d/'token').write_text('synthetic-task-token');agentenv=['HOME=/tmp/probe','PI_OFFLINE=1','DO_NOT_TRACK=1','PRIME_AGENT_CODING_AGENT_DIR=/tmp/probe/config','PRIME_AGENT_KERNEL_PYTHON=/opt/worker/prime/prime-agent-runtime/.venv/bin/python','PRIME_AGENT_DAEMON_SOCKET=/tmp/probe/daemon.sock']
  cmd=[DOCKER,'--host',host,'exec','-i','-e','LLM_API_KEY',cid,'env',*agentenv,'prime-agent','--mode','daemon','--daemon-socket','/tmp/probe/daemon.sock']
  # Quote the template using JSON rules rather than hand escaping HCL.
  template='{{ with secret "secret/data/runtime" }}{{ .Data.data.LLM_API_KEY }}{{ end }}'
  hcl='vault { address = '+json.dumps(f'http://127.0.0.1:{srv.server_port}')+' }\nauto_auth { method "token_file" { config = { token_file_path = '+json.dumps(str(d/'token'))+' } } }\nenv_template "LLM_API_KEY" { contents = '+json.dumps(template)+'\n error_on_missing_key = true }\nexec { command = '+json.dumps(cmd)+'\n restart_on_secret_changes = "never" }\ntemplate_config { exit_on_retry_failure = true }'
  (d/'agent.hcl').write_text(hcl)
  log=(d/'supervisor.log').open('w+')
  supervisor=subprocess.Popen([BAO,'agent','-config='+str(d/'agent.hcl'),'-log-level=error'],stdin=subprocess.DEVNULL,stdout=log,stderr=log,env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':tmp})
  deadline=time.monotonic()+30
  while docker('exec',cid,'test','-S','/tmp/probe/daemon.sock',check=False).returncode:
   assert supervisor.poll() is None,'Bao supervisor exited';assert time.monotonic()<deadline,'socket timeout';time.sleep(.1)
  acp=subprocess.Popen([DOCKER,'--host',host,'exec','-i',cid,'env','-u','LLM_API_KEY','-u','BAO_TOKEN','-u','VAULT_TOKEN',*agentenv,'prime-agent','--mode','acp','--offline','--provider','fixture','--model','offline-check','--cwd','/tmp/probe/work'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
  events=queue.Queue();bad=[]
  def read():
   for line in acp.stdout:
    try:events.put(json.loads(line))
    except ValueError:bad.append(line)
  threading.Thread(target=read,daemon=True).start()
  def rpc(i,method,params):
   acp.stdin.write(json.dumps({'jsonrpc':'2.0','id':i,'method':method,'params':params})+'\n');acp.stdin.flush();deadline=time.monotonic()+30
   while time.monotonic()<deadline:
    v=events.get(timeout=max(.1,deadline-time.monotonic()))
    if v.get('id')==i:return v
   raise RuntimeError('rpc timeout')
  assert 'result' in rpc(1,'initialize',{'protocolVersion':1,'clientCapabilities':{},'clientInfo':{'name':'synthetic','version':'1'}})
  created=rpc(2,'session/new',{'cwd':'/tmp/probe/work','mcpServers':[]});assert 'result' in created,created
  sid=created['result']['sessionId'];turn=rpc(3,'session/prompt',{'sessionId':sid,'prompt':[{'type':'text','text':'Return a short acknowledgement; synthetic offline test.'}]})
  auth=json.loads(docker('exec',cid,'cat','/tmp/probe/provider-auth.json').stdout)
  assert auth['authorization_correct'] and not bad and turn.get('result',{}).get('stopReason')=='end_turn','synthetic ACP/auth contract failed'
  scan=docker('exec',cid,'python3','-c',"import pathlib;root=pathlib.Path('/tmp/probe');assert not any(b'synthetic-key' in p.read_bytes() for p in root.rglob('*') if p.is_file() and p.name!='provider.py')")
  print(json.dumps({'prime':'0.10.1','bao':'2.5.4','daemon_socket':True,'acp_non_json_lines':len(bad),'synthetic_provider_auth_correct':auth['authorization_correct'],'turn_has_result':'result' in turn,'no_persisted_runtime_key':True,'paid_model_calls':0,'note':'Docker exec is fixture transport only, no guest container architecture'}))
  acp.stdin.close();acp.wait(timeout=10)
finally:
 if supervisor is not None and supervisor.poll() is None:
  supervisor.terminate()
  try:supervisor.wait(timeout=10)
  except subprocess.TimeoutExpired:supervisor.kill();supervisor.wait(timeout=5)
 srv.shutdown();srv.server_close();subprocess.run([DOCKER,'rm','-f',cid],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
