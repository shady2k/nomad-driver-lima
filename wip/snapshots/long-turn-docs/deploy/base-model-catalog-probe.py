"""Actual Prime/acpx control acceptance; localhost synthetic endpoint, no prompts."""
def advertised_values(session):
 values=[m['modelId']for m in session.get('models',{}).get('availableModels',[])]
 for option in session.get('configOptions',[]):
  if option.get('id')=='model':
   for item in option.get('options',[]):
    if 'value'in item:values.append(item['value'])
    else:values.extend(x['value']for x in item.get('options',[]))
 return values

def probe(provider):
 import copy,http.server,json,os,pathlib,queue,shlex,signal,subprocess,tempfile,threading,time,uuid
 import importlib.util,hashlib
 source=pathlib.Path('/opt/nomad-workers/task-main.py');assert hashlib.sha256(source.read_bytes()).hexdigest()=='e90542153acbf07ce653d2ee546a4c4d0745f6c62e1abbe97200db062544655c'
 spec=importlib.util.spec_from_file_location('accepted_guest_model_generator',source);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);native,selected=module.native_gateway(provider,str(uuid.uuid4()))
 expected={json.dumps([provider,m['id']],separators=(',',':')):m['id']for provider,p in native['providers'].items()for m in p['models']}
 assert len(expected)==10
 hits=[]
 class Handler(http.server.BaseHTTPRequestHandler):
  def log_message(self,*args):pass
  def do_POST(self):hits.append(1);self.send_response(500);self.end_headers()
 server=http.server.HTTPServer(('127.0.0.1',0),Handler);threading.Thread(target=server.serve_forever,daemon=True).start()
 with tempfile.TemporaryDirectory(prefix='model-catalog-accept-')as td:
  root=pathlib.Path(td);config=root/'config';config.mkdir();work=root/'work';work.mkdir();sock=root/'daemon.sock';fixture=copy.deepcopy(native)
  for p in fixture['providers'].values():p['baseUrl']='http://127.0.0.1:'+str(server.server_port)+'/v1';p['apiKey']='LLM_API_KEY'
  (config/'models.json').write_text(json.dumps(fixture))
  env={'LLM_API_KEY':'PUBLIC_SYNTHETIC_NOT_A_CREDENTIAL','PATH':'/usr/local/bin:/usr/bin:/bin','HOME':td,'PI_OFFLINE':'1','DO_NOT_TRACK':'1','PRIME_AGENT_CODING_AGENT_DIR':str(config),'PRIME_AGENT_KERNEL_PYTHON':'/opt/worker/prime/prime-agent-runtime/.venv/bin/python','PRIME_AGENT_DAEMON_SOCKET':str(sock)}
  daemonlog=(root/'daemon.log').open('w');daemon=subprocess.Popen(['prime-agent','--mode','daemon','--daemon-socket',str(sock)],env=env,stdin=subprocess.DEVNULL,stdout=daemonlog,stderr=daemonlog,start_new_session=True)
  child=None;base=None;name='catalog-'+str(uuid.uuid4());acks=[]
  first=next(pair for pair,id in expected.items()if id=='deepseek-v4.1-flash-auto');group,model=json.loads(first)
  argv=['prime-agent','--mode','acp','--offline','--daemon-socket',str(sock),'--provider',group,'--model',model,'--cwd',str(work)]
  try:
   deadline=time.monotonic()+30
   while not sock.exists():
    assert daemon.poll()is None and time.monotonic()<deadline,'Prime daemon readiness failure';time.sleep(.05)
   child=subprocess.Popen(argv,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True,start_new_session=True);q=queue.Queue()
   def read():
    for line in child.stdout:
     try:q.put(json.loads(line))
     except ValueError:pass
   threading.Thread(target=read,daemon=True).start()
   def rpc(i,method,params):
    child.stdin.write(json.dumps({'jsonrpc':'2.0','id':i,'method':method,'params':params})+'\n');child.stdin.flush();deadline=time.monotonic()+30
    while time.monotonic()<deadline:
     v=q.get(timeout=max(.1,deadline-time.monotonic()))
     if v.get('id')==i:
      assert 'result'in v,(method,v.get('error'));return v['result']
    raise RuntimeError('ACP timeout')
   rpc(1,'initialize',{'protocolVersion':1,'clientCapabilities':{},'clientInfo':{'name':'native-model-catalog-no-prompt','version':'1'}})
   session=rpc(2,'session/new',{'cwd':str(work),'mcpServers':[]});advertised_raw=advertised_values(session)
   advertised={json.dumps(json.loads(x),separators=(',',':'))for x in advertised_raw};assert set(expected)<=advertised,('Native advertised catalog mismatch',list(session),session.get('configOptions'),advertised_raw,sorted(set(expected)-advertised))
   child.stdin.close();child.wait(timeout=10);child=None
   base=['acpx','--agent',shlex.join(argv),'--cwd',str(work),'--ttl','10','--timeout','20','--format','json','--prompt-retries','0','--deny-all','--suppress-reads']
   def cli(args):
    r=subprocess.run(base+args,env=env,capture_output=True,text=True,timeout=40);assert r.returncode==0,('acpx control failed',args[0],r.stderr[-1500:]);return [json.loads(x)for x in r.stdout.splitlines()if x.strip()]
   created=cli(['sessions','new','--name',name]);record=next(x['acpxRecordId']for x in created if x.get('acpxRecordId'))
   for selected in ['gpt-6.1-sol','deepseek-v4.1-flash-auto']:
    value=next(pair for pair,id in expected.items()if id==selected);result=cli(['set','model',value,'-s',name]);ack=next(x for x in result if x.get('action')=='model_set');assert ack['modelId']==value and ack['acpxRecordId']==record and ack['resumed']is False;acks.append(selected)
   cli(['sessions','close',name]);base=None
   assert not hits,'Unexpected model request'
   result={'schema_version':1,'native_models_advertised':sorted(expected.values()),'native_model_count':10,'guest_model_generator_verified':True,'compatibility_groups':len(native['providers']),'acp_session_new':'pass','actual_acpx_control':'pass','coordinator_selections':acks,'worker_selector_catalog_validation':True,'worker_tasks_started':0,'model_requests':0,'endpoint_fixture':'localhost-only-no-real-gateway','credentials':'public-synthetic-fixture-only'}
  finally:
   if base is not None:
    subprocess.run(base+['sessions','close',name],env=env,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=30)
   if child is not None and child.poll()is None:
    child.terminate();child.wait(timeout=10)
   if daemon.poll()is None:
    os.killpg(daemon.pid,signal.SIGTERM)
    try:daemon.wait(timeout=10)
    except subprocess.TimeoutExpired:os.killpg(daemon.pid,signal.SIGKILL);daemon.wait(timeout=10)
   server.shutdown();server.server_close();daemonlog.close()
  result['prime_daemon_and_direct_transport_stopped']=True;result['acpx_session_closed']=True;print(json.dumps(result))
