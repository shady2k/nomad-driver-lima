#!/usr/bin/env python3
"""One-shot guest bootstrap of the official Prime daemon. JSON stdin only.
Only the selected provider key enters daemon memory. No bridge or credential file.
"""
import json,os,pathlib,resource,subprocess,sys,time
resource.setrlimit(resource.RLIMIT_CORE,(0,0));os.umask(0o077)
p=json.load(sys.stdin);root=pathlib.Path(p['state_dir'])/p['session_id'];root.mkdir(parents=True,exist_ok=True,mode=0o700)
assert root.stat().st_uid==os.getuid() and not root.stat().st_mode&0o077
config=root/'prime-config';home=root/'prime-home';config.mkdir(mode=0o700,exist_ok=True);home.mkdir(mode=0o700,exist_ok=True)
key=p.pop('key');assert isinstance(key,str) and key and len(key)<8192 and all(33<=ord(c)<=126 for c in key)
models=p['models'];assert len(models['providers'])==1 and next(iter(models['providers'].values()))['apiKey']=='LLM_API_KEY'
for name,value in [('models.json',models),('settings.json',p['settings'])]:
 f=config/name;f.write_text(json.dumps(value));f.chmod(0o600)
sock=root/'prime.sock';receipt=root/'prime-daemon.json';assert not receipt.exists() and not sock.exists(),'bootstrap already attempted; inspect owner before retry'
env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':str(home),'PI_OFFLINE':'1','DO_NOT_TRACK':'1','LLM_API_KEY':key,'PRIME_AGENT_CODING_AGENT_DIR':str(config),'PRIME_AGENT_KERNEL_PYTHON':'/opt/worker/prime/prime-agent-runtime/.venv/bin/python','PRIME_AGENT_DAEMON_SOCKET':str(sock)}
# Stdio is not a provider/config dump; the ordinary task emits lifecycle logs.
proc=subprocess.Popen(['/usr/local/bin/prime-agent','--mode','daemon','--daemon-socket',str(sock)],env=env,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True)
receipt.write_text(json.dumps({'pid':proc.pid,'session_id':p['session_id'],'socket':str(sock),'credential_on_disk':False}));receipt.chmod(0o600)
del key,env,p
end=time.monotonic()+30
while not sock.exists():
 if proc.poll() is not None or time.monotonic()>end:raise SystemExit('OFFICIAL_PRIME_DAEMON_NOT_READY_NO_AUTOMATIC_RETRY')
 time.sleep(.1)
print(json.dumps({'stage':'official_prime_daemon_ready','pid':proc.pid,'credential_on_disk':False}),flush=True)
