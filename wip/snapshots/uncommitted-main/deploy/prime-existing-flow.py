#!/usr/bin/env python3
"""Linux-host foreground transport for the existing Prime guest bootstrap.
Run as root via existing SSH/sudo. No listener, host daemon, new credential or prompt
retry. Envconsul selects only LLM_API_KEY using the existing host OpenBao identity.
"""
import argparse,json,os,pathlib,re,resource,shlex,signal,subprocess,sys,tempfile,threading,uuid

NAMES=['provision.sh','uv.lock','package.json','package-lock.json','models.json','settings.json','guest.py','server.mjs','session.mjs','artifacts.py','session-policy.json']
p=argparse.ArgumentParser(description=__doc__)
for k in ['legacy-root','lima-home','limactl','runtime-user','instance','allocation-id','session-id','expected-model']:
    p.add_argument('--'+k,required=True)
p.add_argument('--secret-child',action='store_true')
p.add_argument('--bounded-canary',action='store_true')
p.add_argument('--allow-prompts',action='store_true',help='Only after separate tariff/budget approval; default forbids /prompt')
a=p.parse_args()
assert os.geteuid()==0
assert str(uuid.UUID(a.session_id))==a.session_id
assert re.fullmatch(r'nomad-[a-f0-9]{24}',a.instance)
assert a.runtime_user!='root'
resource.setrlimit(resource.RLIMIT_CORE,(0,0))
root=pathlib.Path(a.legacy_root);comp=root/'current/companion'
node=json.loads((root/'node.json').read_text())
ledgers=[json.loads(f.read_text()) for f in (pathlib.Path(a.lima_home).parent/'driver-state').glob('*.json')]
bound=[v for v in ledgers if v.get('AllocID')==a.allocation_id and v.get('Instance')==a.instance]
assert len(bound)==1 and not bound[0]['Destroyed'] and bound[0]['Phase']=='running' and bound[0]['TaskName']=='worker','Owned running allocation ledger mismatch'
if not a.secret_child:
    original=(comp/'envconsul.hcl').read_text()
    paths=re.findall(r'secret\s*\{\s*path\s*=\s*("[^"\n]+")',original)
    assert len(paths)==1,'Expected the existing single worker secret path'
    path=json.loads(paths[0]);assert path and '\n' not in path
    state=pathlib.Path(a.lima_home).parent/'prime-flow';state.mkdir(mode=0o700,exist_ok=True)
    fd,name=tempfile.mkstemp(prefix='selected-envconsul-',suffix='.hcl',dir=state)
    hcl=('log_level = "warn"\npristine = true\nvault {\n renew_token = false\n retry { enabled = false }\n ssl { verify = true }\n vault_agent_token_file = '+json.dumps(node['vault_token_file'])+'\n}\nsecret {\n path = '+json.dumps(path)+'\n no_prefix = true\n key { name = "LLM_API_KEY" }\n}\nexec { env { allowlist = ["LLM_API_KEY"] } }\n')
    with os.fdopen(fd,'w') as f:f.write(hcl)
    os.chmod(name,0o600)
    command=[sys.executable,'-I','-u',str(pathlib.Path(__file__).resolve()),*sys.argv[1:],'--secret-child']
    env={'HOME':'/root','PATH':'/usr/local/bin:/usr/bin:/bin','VAULT_ADDR':node['vault_addr']}
    binary=node['envconsul_binary']
    os.execve(binary,[binary,'-config='+name,'-once','-vault-renew-token=false','-log-level=warn','--',shlex.join(command)],env)

assert not any(os.environ.get(k) for k in ['VAULT_TOKEN','NOMAD_TOKEN','BOXLITE_DISPATCHER_SEED']),'Unexpected extra credential in child environment'
key=os.environ.pop('LLM_API_KEY','')
assert key and key.isascii() and len(key)<=8192 and all(33<=ord(x)<=126 for x in key),'Selected key unavailable'
sys.path.insert(0,str(comp))
import model_policy,acp_bridge
policy=json.loads((comp/'guest/session-policy.json').read_text())
model=model_policy.select_model({},policy);assert model==a.expected_model,'External model differs from reviewed model'
models=json.loads((comp/'guest/models.json').read_text());provider=policy['provider'];selected=models['providers'][provider]
assert selected['apiKey']=='LLM_API_KEY','Existing provider must resolve the same environment field'
models={'providers':{provider:selected}}
models=model_policy.session_models(models,policy,a.session_id)
assets={name:(comp/'guest'/name).read_text() for name in NAMES}
if a.allow_prompts:
    assert a.bounded_canary, 'Paid transport requires explicit bounded canary mode'
if a.bounded_canary:
    chosen=[m for m in selected['models'] if m['id']==model];assert len(chosen)==1
    chosen[0]['maxTokens']=512;chosen[0]['contextWindow']=20000
    models['providers'][provider]['models']=chosen
assets['models.json']=json.dumps(models)
if a.bounded_canary:
    settings=json.loads(assets['settings.json'])
    settings['compaction']={**settings.get('compaction',{}),'enabled':False}
    settings['retry']={**settings.get('retry',{}),'enabled':False,'maxRetries':0,'failover':{'enabled':False,'maxRetries':0}}
    assets['settings.json']=json.dumps(settings)
    policy['turn_timeout_ms']=60000;policy['max_prompt_bytes']=4096
    assets['session-policy.json']=json.dumps(policy)
    needle="'--no-context-files','--no-skills','--no-prompt-templates',"
    assert assets['server.mjs'].count(needle)==1
    assets['server.mjs']=assets['server.mjs'].replace(needle,"'--autonomous-max-turns','3','--autonomous-max-tokens','4096','--autonomous-timeout-ms','60000',"+needle)
# The legacy token field stays transient inside the private SSH pipe; no gateway
# listener or dispatcher seed is needed for the native transport.
envelope={'assets':assets,'key':key,'token':uuid.uuid4().hex+uuid.uuid4().hex,
          'request':{'session_id':a.session_id,'files':{},'commands':[]},
          'model':model,'provider_hosts':acp_bridge.provider_host_aliases()}
cmd=['/usr/sbin/runuser','-u',a.runtime_user,'--','env','LIMA_HOME='+a.lima_home,a.limactl,
     'shell',a.instance,'sudo','-n','/usr/bin/python3','-I','-u','/opt/worker/assets/bootstrap.py']
proc=subprocess.Popen(cmd,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,
                      env={'HOME':'/root','PATH':'/usr/local/bin:/usr/bin:/bin'},start_new_session=True)
proc.stdin.write((json.dumps(envelope)+'\n').encode());proc.stdin.flush();del envelope
secret=key.encode();del key

def redact(line):return line.replace(secret,b'[REDACTED]')
def input_pump():
    try:
        for line in sys.stdin.buffer:
            if not a.allow_prompts:
                req=json.loads(line)
                if (req.get('method'),req.get('path')) not in [('GET','/status'),('POST','/close')]:
                    raise ValueError('Initialization-only transport forbids prompt and other operations')
            proc.stdin.write(line);proc.stdin.flush()
    except Exception:
        if proc.poll() is None:proc.terminate()
    finally:
        try:proc.stdin.close()
        except OSError:pass

def stderr_pump():
    for line in proc.stderr:
        sys.stderr.buffer.write(redact(line));sys.stderr.buffer.flush()
threading.Thread(target=input_pump,daemon=True).start()
threading.Thread(target=stderr_pump,daemon=True).start()
def stop(*_):
    if proc.poll() is None:os.killpg(proc.pid,signal.SIGTERM)
signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
try:
    for line in proc.stdout:
        sys.stdout.buffer.write(redact(line));sys.stdout.buffer.flush()
    raise SystemExit(proc.wait())
finally:stop()
