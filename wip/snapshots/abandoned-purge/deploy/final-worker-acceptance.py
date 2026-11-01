"""One approved installed-caller model transaction. Durable intents prohibit prompt replay."""
import argparse,base64,datetime,hashlib,json,os,pathlib,subprocess,time,uuid
p=argparse.ArgumentParser();p.add_argument('phase',choices=['prepare','create','turn','collect','finish','verify']);p.add_argument('--root',required=True);p.add_argument('--spec',required=True);a=p.parse_args()
spec_path=pathlib.Path(a.spec);st=spec_path.lstat();assert spec_path.is_file()and not spec_path.is_symlink()and st.st_uid==os.getuid()and st.st_mode&0o077==0,'private acceptance spec required'
spec=json.loads(spec_path.read_text());root=pathlib.Path(a.root);assert root.is_absolute()and str(root)==spec['operation_root'];root.mkdir(parents=True,exist_ok=True,mode=0o700)
assert os.getuid()==pathlib.Path(spec['caller_home']).stat().st_uid
cli=spec['caller_cli'];config=pathlib.Path(spec['catalog_config']);c=json.loads(config.read_text());assert c['namespace']==spec['namespace']
v=c['guest']['prime_vault'];assert c['guest']['model'] is None and v['provider']==spec['expected_provider']
assert c['storage']==spec['expected_storage']
def save(name,d):
 f=root/name;temp=f.with_suffix(f.suffix+'.tmp');fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w')as q:json.dump(d,q,ensure_ascii=False,indent=2);q.write('\n');q.flush();os.fsync(q.fileno())
 os.replace(temp,f)
def load(n):return json.loads((root/n).read_text())
def stage(s,**kw):
 d={'stage':s,'time':datetime.datetime.now(datetime.timezone.utc).isoformat(),**kw};print(json.dumps(d,ensure_ascii=False),flush=True)
 with (root/'stages.jsonl').open('a')as f:os.chmod(f.name,0o600);f.write(json.dumps(d,ensure_ascii=False)+'\n');f.flush()
if not (root/'identity.json').exists():save('identity.json',{'session':str(uuid.uuid4()),'request':str(uuid.uuid4()),'max_model_requests':1})
ids=load('identity.json');session=ids['session'];request=ids['request']
def command(args,target=None,timeout=180):
 cmd=[cli,*(['--config',str(target)]if target else []),*args]
 r=subprocess.run(cmd,capture_output=True,text=True,timeout=timeout)
 # Only this synthetic request is permitted here. Protocol logs remain private.
 stamp=str(time.time_ns());f=root/('cli-'+args[0]+'-'+stamp+'.jsonl');f.write_text(r.stdout);f.chmod(0o600)
 if r.returncode:
  try:code=json.loads(r.stderr.strip())['error']
  except Exception:code='CALLER_COMMAND_FAILED_RAW_OUTPUT_SUPPRESSED'
  save('last-error.json',{'command':args[0],'code':code,'outcome':'inspect_same_operation_no_resubmit'})
  raise RuntimeError(code)
 return [json.loads(x)for x in r.stdout.splitlines()if x.strip()]
def target():return pathlib.Path(load('target.json')['target_config'])
def nomad(args,body=None,timeout=35):
 r=subprocess.run([c['nomad_bin'],*args],input=body,capture_output=True,timeout=timeout)
 if r.returncode:raise RuntimeError('NATIVE_CALLER_NOMAD_FAILED')
 return r.stdout
def allocation():return json.loads(nomad(['alloc','status','-json','-namespace=agent-workers',load('target.json')['allocation']]))
def guest(code,value=None):
 t=load('target.json');args=['alloc','exec','-namespace=agent-workers','-task=worker','-i='+('true'if value is not None else 'false'),'-t=false',t['allocation'],'/usr/bin/python3','-c',code,session]
 return nomad(args,None if value is None else json.dumps(value).encode(),timeout=45)
settings={'retry':{'enabled':False,'maxRetries':0,'failover':{'enabled':False,'maxRetries':0},'provider':{'waitForUsage':{'enabled':False,'pauseUntilReset':False}}},'autonomous':{'maxTurns':1,'maxContinuations':0,'maxTokens':512,'timeoutMs':60000},'compaction':{'enabled':False,'agentCallable':False},'autoRefine':{'enabled':False},'harnessRefinements':False,'telemetry':{'enabled':False},'branchSummary':{'skipPrompt':True}}
if a.phase=='prepare':
 cap=command(['capacity'])[-1];nodes=[n for n in cap['nodes']if n['node_id']==spec['node_id']];assert len(nodes)==1 and nodes[0]['free_reservation_mib']>=2176 and nodes[0]['free_cpu_mhz']>=3998
 save('capacity.json',cap)
 if not (root/'request-started.json').exists():
  save('request-started.json',{'session':session,'job':'worker-'+session})
  result=command(['request','--session',session,'--template','agent-base','--template-version',spec['template_version'],'--architecture','amd64','--vcpus','2','--memory-mib','2048','--disk-gib','20','--purpose','final-canary','--task-title','Validate isolated worker request and result lifecycle','--source-agent-name','acceptance-helper','--source-agent-type','operator-fixture']);save('request.json',result[-1])
 deadline=time.monotonic()+150
 while True:
  status=command(['request-status','--session',session])[-1]
  if status.get('allocation'):
   save('target.json',status);al=allocation()
   if al['ClientStatus']=='running'and al.get('TaskStates',{}).get('worker',{}).get('State')=='running':break
  elif any(x.get('status')=='failed'for x in status.get('allocations',[])):raise RuntimeError('ALLOCATION_FAILED_NO_NEW_REQUEST')
  if time.monotonic()>deadline:raise RuntimeError('BOOT_PENDING_QUERY_SAME_REQUEST')
  time.sleep(2)
 code=r'''
import pathlib,sys,json,os,stat,hashlib
s=sys.argv[1];d=pathlib.Path('/var/tmp/nomad-workers')/s
assert d.is_dir()and not d.is_symlink()and d.stat().st_uid==os.getuid()
assert not (d/'runtime-request.json').exists(),'Runtime already requested; inspect existing settings'
r=d/'prime-config';r.mkdir(mode=0o700,exist_ok=True);assert not r.is_symlink()and stat.S_IMODE(r.stat().st_mode)==0o700
v=json.load(sys.stdin);p=r/'settings.json';b=(json.dumps(v,sort_keys=True)+'\n').encode()
if p.exists():assert p.read_bytes()==b
else:
 fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'wb')as f:f.write(b);f.flush();os.fsync(f.fileno())
print(json.dumps({'settings_sha256':hashlib.sha256(b).hexdigest(),'retry_enabled':False,'failover_enabled':False,'max_turns':1,'max_output_tokens':512}))
'''
 proof=json.loads(guest(code,settings));save('bounded-settings.json',proof);stage('one_vm_running_bounded_settings_prepared',session=session,allocation=status['allocation'],model_prompts=0,**proof)
elif a.phase=='create':
 if (root/'created.json').exists():stage('already_created_no_relaunch',session=session)
 else:
  result=command(['create','--session',session],target());save('created.json',result[-1]);stage('native_prime_acpx_session_ready',session=session,model_prompts=0)
elif a.phase=='turn':
 assert (root/'created.json').exists()
 if (root/'response.json').exists():stage('model_turn_already_saved_no_resubmit',session=session);raise SystemExit(0)
 prompt='Ответь одной строкой: Проверка воркера завершена. Не используй инструменты и не читай файлы.'
 f=root/'prompt.txt'
 if f.exists():assert f.read_text()==prompt
 else:f.write_text(prompt);f.chmod(0o600)
 if not (root/'submit-started.json').exists():
  save('submit-started.json',{'session':session,'request':request,'prompt_sha256':hashlib.sha256(prompt.encode()).hexdigest(),'allowed_model_requests':1})
  result=command(['submit','--session',session,'--request',request,'--prompt-file',str(f)],target(),timeout=60);save('submitted.json',result[-1]);stage('single_prompt_dispatched',session=session,request=request)
 elif not (root/'submitted.json').exists():raise RuntimeError('SUBMIT_OUTCOME_UNKNOWN_DO_NOT_RESUBMIT_INSPECT_STATUS')
 frames=command(['watch','--session',session,'--request',request],target(),timeout=150);save('watch.json',frames)
 terminal=[x['event']['result']for x in frames if x.get('event',{}).get('type')=='turn_result'];assert terminal and terminal[-1]['status']=='completed','MODEL_TURN_NOT_COMPLETED_NO_REPLAY'
 text='';tool_events=0;usage=[]
 for frame in frames:
  e=frame.get('event',{});msg=e.get('message',{});u=msg.get('params',{}).get('update',{})
  if u.get('sessionUpdate')=='agent_message_chunk'and u.get('content',{}).get('type')=='text':text+=u['content']['text']
  if u.get('sessionUpdate')in ['tool_call','tool_call_update']:tool_events+=1
  if 'usage' in str(u.get('sessionUpdate','')).lower():usage.append(u)
 assert text.strip()and len(text.encode())<16384,'MODEL_RESPONSE_ABSENT_OR_OVERSIZE'
 save('response.json',{'text':text,'sha256':hashlib.sha256(text.encode()).hexdigest(),'tool_events':tool_events,'terminal':terminal[-1],'usage_events':usage});assert tool_events==0,'UNEXPECTED_TOOL_EVENT_NO_FURTHER_PROMPT'
 stage('one_real_model_response_received',session=session,request=request,response=text,tool_events=tool_events,usage_available=bool(usage))
elif a.phase=='collect':
 r=load('response.json');report={'schema_version':1,'kind':'synthetic_worker_acceptance','session':session,'request':request,'model':v['provider']['model'],'gateway':v['provider']['base_url'],'model_prompts_submitted':1,'tool_events':r['tool_events'],'max_output_tokens':512,'retry_enabled':False,'failover_enabled':False,'response_sha256':r['sha256']}
 code=r'''
import pathlib,json,sys,os,hashlib
v=json.load(sys.stdin);root=pathlib.Path('/workspace/repo');assert root.is_dir()and not root.is_symlink()
for name,raw in [('model-response.txt',v['text'].encode()),('worker-acceptance.json',(json.dumps(v['report'],sort_keys=True)+'\n').encode())]:
 p=root/name
 if p.exists():assert not p.is_symlink()and p.read_bytes()==raw
 else:
  fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'wb')as f:f.write(raw);f.flush();os.fsync(f.fileno())
print(json.dumps({'declared_artifacts_saved':2}))
'''
 guest(code,{'text':r['text'],'report':report})
 m=command(['collect','--session',session,'--request',request,'model-response.txt','worker-acceptance.json'],target(),timeout=180)[-1];assert m['actual_backend']=='s3'and any(f['path']=='.nomad-logs/diagnostics-index.json'for f in m['files']);save('manifest.json',m)
 stage('declared_response_report_redacted_native_logs_s3_verified',session=session,files=len(m['files']),s3_manifest_sha256=m['receipt']['manifest_sha256'])
elif a.phase=='finish':
 assert (root/'manifest.json').exists();r=command(['finish','--session',session,'--request',request],target(),timeout=90)[-1];save('finished.json',r)
 al=allocation();phases={x.get('DriverMessage')for x in al['TaskStates']['worker']['Events']};assert {'lima task_token_revoked','lima vm_stopped'}<=phases
 save('terminal-proof.json',{'allocation':al['ID'],'job':'worker-'+session,'client_status':al['ClientStatus'],'task_state':al['TaskStates']['worker']['State'],'task_token_revoked':True,'vm_stopped':True,'disk_purged':False})
 stage('native_finish_token_revoked_vm_stopped_disk_retained',session=session,allocation=al['ID'])
else:
 assert load('terminal-proof.json')['vm_stopped'];m=load('manifest.json');t=json.loads(target().read_text());requestfile=pathlib.Path(t['receipt_root'])/session/(request+'.json');state=json.loads(requestfile.read_text());out=requestfile.parent/state['manifest_dir'];item=next(f for f in m['files']if f['path']=='model-response.txt');src=out/item['local_file'];saved=root/'response-before-restore.bin'
 if not saved.exists():os.rename(src,saved)
 result=command(['result','--session',session,'--request',request],target(),timeout=120)[-1];assert result==m and src.read_bytes()==saved.read_bytes()and hashlib.sha256(src.read_bytes()).hexdigest()==load('response.json')['sha256']
 save('summary.json',{'session':session,'request':request,'allocation':load('target.json')['allocation'],'job':'worker-'+session,'real_model_response':load('response.json')['text'],'model_prompts_submitted':1,'tool_events':load('response.json')['tool_events'],'s3_manifest_sha256':m['receipt']['manifest_sha256'],'response_sha256':load('response.json')['sha256'],'native_logs_redacted_and_exported':True,'restored_from_s3_after_vm_stop':True,'task_token_revoked':True,'vm_stopped':True,'disk_retained':True,'exact_gateway_charge_available':False,'usage_events':load('response.json')['usage_events']})
 stage('final_model_cycle_pass_output_restored_after_stop',**load('summary.json'))
