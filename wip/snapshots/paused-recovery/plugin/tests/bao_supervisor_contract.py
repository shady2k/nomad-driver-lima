"""Opt-in official Bao 2.5.4 + loopback synthetic API; no model or real credentials."""
import json,threading,tempfile,pathlib,subprocess,sys,os
from http.server import BaseHTTPRequestHandler,HTTPServer
class H(BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_GET(self):
  if self.path.startswith('/v1/sys/internal/ui/mounts'):v={'data':{'type':'kv','path':'secret/','options':{'version':'2'}}}
  elif self.path.startswith('/v1/sys/health'):v={'initialized':True,'sealed':False,'version':'2.5.4'}
  elif self.path.startswith('/v1/auth/token/lookup-self'):v={'data':{'id':'synthetic-task-token','ttl':3600,'renewable':True,'type':'service','policies':['synthetic']}}
  else:v={'data':{'data':{'LLM_API_KEY':'synthetic-key'}}}
  b=json.dumps(v).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(b)
s=HTTPServer(('127.0.0.1',0),H);threading.Thread(target=s.serve_forever,daemon=True).start()
with tempfile.TemporaryDirectory() as tmp:
 d=pathlib.Path(tmp);(d/'token').write_text('synthetic-task-token')
 cfg={'vault':{'address':f'http://127.0.0.1:{s.server_port}'},'auto_auth':{'method':{'token_file':{'config':{'token_file_path':str(d/'token')}}}},'env_template':{'LLM_API_KEY':{'contents':'{{ with secret "secret/data/runtime" }}{{ .Data.data.LLM_API_KEY }}{{ end }}','error_on_missing_key':True}},'exec':{'command':[sys.executable,'-c','import os;assert os.environ["LLM_API_KEY"]=="synthetic-key";print("child-ready",flush=True);raise SystemExit(7)'],'restart_on_secret_changes':'never'},'template_config':{'exit_on_retry_failure':True}}
 (d/'agent.json').write_text('vault { address = '+json.dumps(cfg['vault']['address'])+' }\nauto_auth { method "token_file" { config = { token_file_path = '+json.dumps(str(d/'token'))+' } } }\nenv_template "LLM_API_KEY" { contents = '+json.dumps(cfg['env_template']['LLM_API_KEY']['contents'])+'\n error_on_missing_key = true }\nexec { command = '+json.dumps(cfg['exec']['command'])+'\n restart_on_secret_changes = "never" }\ntemplate_config { exit_on_retry_failure = true }')
 try:p=subprocess.run([os.environ.get('BAO_BIN','bao'),'agent','-config='+str(d/'agent.json'),'-log-level=error','-combine-logs=true'],capture_output=True,text=True,timeout=15,env={"PATH":"/usr/bin:/bin","HOME":tmp})
 except subprocess.TimeoutExpired as e:
  print('timeout stdout',e.stdout,'stderr',e.stderr);raise
 assert p.returncode==7 and 'child-ready' in p.stdout,(p.returncode,'synthetic child did not complete')
 assert p.stdout!='child-ready\n','upstream stdout behavior changed; review direct ACP feasibility'
 print(json.dumps({'official_bao':'2.5.4','model_calls':0,'child_exit':p.returncode,'child_injection':True,'stdout_contains_supervisor_banner':'OpenBao Agent configuration' in p.stdout,'direct_acp_stdout_safe':False}))
s.shutdown()
