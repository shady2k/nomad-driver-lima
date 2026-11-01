#!/usr/bin/env python3
"""One-shot client for the existing private Prime JSON RPC over SSH/Lima stdio.
No listener, prompt replay or key reader. Default initializes/closes with zero prompts.
"""
import argparse,base64,hashlib,json,os,pathlib,select,shlex,subprocess,time,uuid

p=argparse.ArgumentParser(description=__doc__);p.add_argument('--config',required=True);p.add_argument('--prompt-file')
a=p.parse_args();c=json.loads(pathlib.Path(a.config).read_text());out=pathlib.Path(c['output_dir']);out.mkdir(mode=0o700,parents=True,exist_ok=False);os.umask(0o077)
for k in ['session_id','request_id']:assert str(uuid.UUID(c[k]))==c[k]
command=c['host_command']+['--session-id',c['session_id'],'--bounded-canary']
if a.prompt_file:
    assert c['budget']['approved_total_usd']<=1
    assert c['budget']['input_usd_per_million']==0.15 and c['budget']['output_usd_per_million']==0.60
    prompt=pathlib.Path(a.prompt_file).read_text();assert 0<len(prompt.encode())<=4096
    command+=['--allow-prompts']
err=open(out/'transport.stderr','xb');proc=subprocess.Popen(c['ssh']+[shlex.join(command)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=err)
events=open(out/'events.jsonl','xb');buffer=b''
def save(name,value):
    path=out/name;path.write_text(json.dumps(value,indent=2)+'\n');path.chmod(0o600)
def receive(deadline):
    global buffer
    while time.monotonic()<deadline:
        if b'\n' in buffer:
            line,buffer=buffer.split(b'\n',1)
            if not line:continue
            events.write(line+b'\n');events.flush()
            try:return json.loads(line)
            except ValueError:continue
        ready,_,_=select.select([proc.stdout],[],[],min(1,max(0,deadline-time.monotonic())))
        if ready:
            chunk=os.read(proc.stdout.fileno(),65536)
            if not chunk:raise RuntimeError('Transport closed; no automatic retry')
            buffer+=chunk
            if len(buffer)>16777216:raise RuntimeError('Frame limit')
    raise TimeoutError('Outcome unknown; do not resend')
def until(test,seconds=120):
    end=time.monotonic()+seconds
    while True:
        item=receive(end)
        if test(item):return item
        if item.get('event')=='guest_bootstrap_failed':raise RuntimeError('Native bootstrap failed; inspect categorical diagnostics')
def rpc(path,method='GET',body=None,seconds=120):
    rid=str(uuid.uuid4());proc.stdin.write((json.dumps({'rpc_id':rid,'path':path,'method':method,'body':body or {}})+'\n').encode());proc.stdin.flush()
    answer=until(lambda x:x.get('rpc_id')==rid,seconds)
    assert answer['status'] in [200,202], 'RPC rejected; do not resend'
    return answer['body']
try:
    ready=until(lambda x:x.get('event')=='guest_session_ready',180)
    status=rpc('/status');assert status['state']=='ready' and status['request_count']==0 and status['model']==c['model']
    save('initialize.json',{'ready':ready,'status':status,'prompt_count':0,'usage_verified':False})
    print(json.dumps({'phase':'native_initialized','session_id':c['session_id'],'model':status['model'],'prompt_count':0}),flush=True)
    if a.prompt_file:
        request=c['request_id'];intent={'request_id':request,'prompt_sha256':hashlib.sha256(prompt.encode()).hexdigest(),'automatic_retry':False}
        save('prompt.intent.json',intent)
        accepted=rpc('/prompt','POST',{'request_id':request,'prompt':prompt});save('accepted.json',accepted)
        # This is a single public request. Stock Prime's three-turn/max-token and
        # timeout launch limits bound the isolated canary; provider retries are off.
        until(lambda x:x.get('event')=='request_terminal' and x.get('request_id')==request,120)
        result=rpc('/result/'+request);save('result.json',result)
        assert result['state'] in ['completed','cancelled'],'Turn failed or unknown; do not retry'
        assert result['text'].strip(),'No model response'
        exported=rpc('/artifacts','POST',{'paths':[result['result_file']]})
        manifests=[]
        for i,f in enumerate(exported['files']):
            raw=base64.b64decode(f['base64']);assert len(raw)==f['size'] and hashlib.sha256(raw).hexdigest()==f['sha256']
            local='guest-result-'+str(i)+'.json';(out/local).write_bytes(raw)
            assert json.loads(raw)['text']==result['text']
            manifests.append({'guest_path':f['path'],'local_file':local,'bytes':len(raw),'sha256':f['sha256']})
        raw=result['text'].encode();(out/'response.txt').write_bytes(raw)
        save('manifest.json',{'session_id':c['session_id'],'request_id':request,'files':manifests,'response_sha256':hashlib.sha256(raw).hexdigest(),'verified_before_close':True})
        print(json.dumps({'phase':'result_verified','request_id':request,'text':result['text'],'tool_summary':result['tool_summary'],'manifest':str(out/'manifest.json')}),flush=True)
    closed=rpc('/close','POST',{});save('closed.json',closed)
    proc.stdin.close();code=proc.wait(timeout=30);assert code==0,'Foreground bootstrap did not close cleanly'
    print(json.dumps({'phase':'session_closed','exit_code':code,'output_dir':str(out)}),flush=True)
finally:
    if proc.poll() is None:proc.terminate()
    events.close();err.close()
