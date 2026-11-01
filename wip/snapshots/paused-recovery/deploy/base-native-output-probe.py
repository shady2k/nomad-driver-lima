#!/usr/bin/env python3
"""No-model synthetic native-output probe inside an owned clean acceptance VM."""
import hashlib,json,os,pathlib,queue,signal,subprocess,tempfile,threading,time,uuid
with tempfile.TemporaryDirectory(prefix='base-native-output-')as td:
 root=pathlib.Path(td);sid=str(uuid.uuid4());session=root/'state'/sid
 check=root/'check.py';check.write_text('#!/usr/bin/python3\nimport json\nprint(json.dumps({"fixture_runtime_check":True,"model_requests":0}),flush=True)\n');check.chmod(0o700)
 p=subprocess.Popen(['/usr/bin/python3','/opt/nomad-workers/task-main.py','--state-dir',str(root/'state'),'--session',sid,'--runtime-check',str(check)],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 q=queue.Queue();frames=[];errors=[]
 def reader(stream,target):
  for line in stream:target.put(line)if hasattr(target,'put')else target.append(line)
 threading.Thread(target=reader,args=(p.stdout,q),daemon=True).start();threading.Thread(target=reader,args=(p.stderr,errors),daemon=True).start()
 def until(predicate,timeout=20):
  end=time.monotonic()+timeout
  while time.monotonic()<end:
   try:line=q.get(timeout=.2)
   except queue.Empty:
    assert p.poll()is None,'Probe main exited before expected output';continue
   d=json.loads(line);frames.append(d)
   if predicate(d):return
  raise RuntimeError('Native output probe timed out')
 try:
  until(lambda d:d.get('stage')=='guest_runtime_ready')
  operation=str(uuid.uuid4());pieces=['chunk-%03d:'%i+'x'*700+'\n'for i in range(96)]
  log=session/'lifecycle.jsonl'
  with log.open('w')as f:
   for chunk in pieces:f.write(json.dumps({'component':'agent-operation','stage':'operation_output','session_id':sid,'operation_id':operation,'label':'prepare-check','state':'running','stream':'stdout','text':chunk})+'\n')
   f.write(json.dumps({'component':'agent-operation','stage':'operation_output','session_id':sid,'operation_id':operation,'label':'prepare-check','state':'running','stream':'stderr','text':'LLM_API_KEY=PUBLIC_SYNTHETIC_NOT_A_CREDENTIAL\n'})+'\n');f.flush();os.fsync(f.fileno())
  until(lambda d:d.get('component')=='agent-operation'and d.get('stream')=='stderr')
  actual=''.join(d['text']for d in frames if d.get('component')=='agent-operation'and d.get('stream')=='stdout');assert actual==''.join(pieces)
  err=''.join(d['text']for d in frames if d.get('component')=='agent-operation'and d.get('stream')=='stderr');assert 'PUBLIC_SYNTHETIC_NOT_A_CREDENTIAL'not in err and '[REDACTED]'in err
  result={'schema_version':1,'native_output_frames':len(pieces),'native_stdout_bytes':len(actual.encode()),'native_stdout_sha256':hashlib.sha256(actual.encode()).hexdigest(),'all_frames_complete_in_order':True,'synthetic_credential_label_redacted':True,'model_requests':0}
 finally:
  if p.poll()is None:p.send_signal(signal.SIGTERM)
  try:rc=p.wait(timeout=10)
  except subprocess.TimeoutExpired:p.kill();p.wait();raise RuntimeError('Probe task did not stop normally')
 assert rc==143 and not errors,('Probe task exit',rc)
 result['probe_task_stopped']=True;print(json.dumps(result))
