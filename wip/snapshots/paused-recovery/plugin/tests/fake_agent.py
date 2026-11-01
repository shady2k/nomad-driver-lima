# Offline ACP fixture only, not a deployed agent or model implementation.
import sys,json,uuid,threading,time,os,pathlib
lock=threading.Lock()
def trace(method):
 fd=os.open('/tmp/methods.jsonl',os.O_WRONLY|os.O_APPEND|os.O_CREAT,0o600);os.write(fd,(json.dumps({'method':method,'pid':os.getpid()})+'\n').encode());os.close(fd)
def send(v):
 with lock:print(json.dumps(v),flush=True)
def answer(m):
 method=m.get('method');trace(method)
 if method=='initialize':result={'protocolVersion':1,'agentCapabilities':{'loadSession':False,'promptCapabilities':{}},'agentInfo':{'name':'mock-prime-capability-fixture','version':'1'}}
 elif method=='session/new':result={'sessionId':'fixture-'+str(uuid.uuid4())}
 elif method=='session/prompt':
  sid=m['params']['sessionId']
  for i in range(5):
   time.sleep(1);send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':sid,'update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':'fixture-step-'+str(i)+'\n'}}}})
  pathlib.Path('/tmp/work/result.txt').write_text('synthetic result, no model\n');pathlib.Path('/tmp/work/report.json').write_text(json.dumps({'tests':'fixture-pass','model_calls':0}))
  result={'stopReason':'end_turn'}
 elif method=='session/cancel':return
 else:
  if 'id'in m:send({'jsonrpc':'2.0','id':m['id'],'error':{'code':-32601,'message':'Method not found'}})
  return
 if 'id'in m:send({'jsonrpc':'2.0','id':m['id'],'result':result})
for line in sys.stdin:
 m=json.loads(line)
 if m.get('method')=='session/prompt':threading.Thread(target=answer,args=(m,),daemon=True).start()
 else:answer(m)
