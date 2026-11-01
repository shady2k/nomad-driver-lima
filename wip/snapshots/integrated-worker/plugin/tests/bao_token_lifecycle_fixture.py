"""Synthetic-only upstream token-file lifecycle; no real secrets or model calls."""
import sys,json,os,pathlib,subprocess,tempfile,threading,time,urllib.request,shutil
BAO=shutil.which(os.environ.get("BAO_BIN","bao"));assert BAO
from http.server import BaseHTTPRequestHandler,HTTPServer
state={'valid':{'synthetic-one':True,'synthetic-two':True},'lookups':[],'renews':[],'reads':[],'revokes':[]}
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def reply(self,v,status=200):
  self.send_response(status);self.end_headers();self.wfile.write(json.dumps(v).encode())
 def do_GET(self):
  token=self.headers.get('X-Vault-Token')
  if self.path.startswith('/v1/sys/health'):return self.reply({'initialized':True,'sealed':False,'version':'2.5.4'})
  if self.path.startswith('/v1/sys/internal/ui/mounts'):return self.reply({'data':{'type':'kv','path':'secret/','options':{'version':'2'}}})
  if self.path.startswith('/v1/auth/token/lookup-self'):state['lookups'].append(token)
  else:state['reads'].append(token)
  if not state['valid'].get(token):return self.reply({'errors':['permission denied']},403)
  if self.path.startswith('/v1/auth/token/lookup-self'):return self.reply({'data':{'id':token,'ttl':2,'renewable':True,'type':'service','policies':['synthetic']}})
  self.reply({'data':{'data':{'LLM_API_KEY':'synthetic-runtime-key'}}})
 def do_POST(self):
  token=self.headers.get('X-Vault-Token');self.rfile.read(int(self.headers.get('Content-Length','0')))
  if self.path.startswith('/v1/auth/token/revoke-self'):
   state['valid'][token]=False;state['revokes'].append(token);return self.reply({})
  state['renews'].append(token)
  if not state['valid'].get(token):return self.reply({'errors':['permission denied']},403)
  self.reply({'auth':{'client_token':token,'lease_duration':2,'renewable':True}})
H.do_PUT=H.do_POST
srv=HTTPServer(('127.0.0.1',0),H);threading.Thread(target=srv.serve_forever,daemon=True).start()
def wait(test,seconds=12):
 end=time.monotonic()+seconds
 while time.monotonic()<end:
  if test():return
  time.sleep(.05)
 raise RuntimeError('synthetic lifecycle observation timeout')
p=None
try:
 with tempfile.TemporaryDirectory() as tmp:
  d=pathlib.Path(tmp);token=d/'token';token.write_text('synthetic-one');started=d/'started';child=d/'child.py'
  child.write_text('import os,signal,time,pathlib\nassert os.environ["LLM_API_KEY"]=="synthetic-runtime-key"\np=pathlib.Path('+repr(str(started))+')\nwith p.open("a") as f:f.write(str(os.getpid())+"\\n")\nsignal.signal(signal.SIGTERM,lambda *a:exit(0))\nwhile True:time.sleep(.1)\n')
  template='{{ with secret "secret/data/runtime" }}{{ .Data.data.LLM_API_KEY }}{{ end }}'
  hcl='vault { address = '+json.dumps(f'http://127.0.0.1:{srv.server_port}')+' }\nauto_auth { method "token_file" { config = { token_file_path = '+json.dumps(str(token))+' } } }\nenv_template "LLM_API_KEY" { contents = '+json.dumps(template)+'\n error_on_missing_key = true }\nexec { command = '+json.dumps([sys.executable,str(child)])+'\n restart_on_secret_changes = "never" }\ntemplate_config { static_secret_render_interval = "1s" }'
  cfg=d/'agent.hcl';cfg.write_text(hcl);log=(d/'supervisor.log').open('w+')
  p=subprocess.Popen([BAO,'agent','-config='+str(cfg),'-log-level=error'],stdin=subprocess.DEVNULL,stdout=log,stderr=log,env={'PATH':'/usr/bin:/bin','HOME':tmp})
  wait(lambda:started.exists());first=started.read_text();wait(lambda:'synthetic-one' in state['renews'])
  replacement=d/'replacement';replacement.write_text('synthetic-two');os.replace(replacement,token);state['valid']['synthetic-one']=False
  wait(lambda:'synthetic-two' in state['lookups']);wait(lambda:'synthetic-two' in state['renews'])
  assert started.read_text()==first and p.poll() is None,'child restarted on token replacement'
  req=urllib.request.Request(f'http://127.0.0.1:{srv.server_port}/v1/auth/token/revoke-self',data=b'{}',headers={'X-Vault-Token':'synthetic-two'},method='POST')
  urllib.request.urlopen(req,timeout=2).close();time.sleep(1.5)
  assert started.read_text()==first and p.poll() is None,'unexpected restart on token revoke'
  print(json.dumps({'official_bao':'2.5.4','agent_renewal_observed':True,'replacement_picked_up_after_old_token_renewal_failure':True,'child_start_count':len(first.splitlines()),'explicit_synthetic_revoke':True,'revoke_does_not_erase_running_child_environment':True,'model_calls':0}))
  p.terminate();p.wait(timeout=10);p=None
finally:
 if p is not None and p.poll() is None:p.terminate();p.wait(timeout=10)
 srv.shutdown();srv.server_close()
