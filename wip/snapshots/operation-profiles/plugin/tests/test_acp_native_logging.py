import importlib.util,pathlib,sys,json,subprocess,tempfile,uuid,unittest
ROOT=pathlib.Path(__file__).parents[2]
spec=importlib.util.spec_from_file_location('native_acp_consumer',ROOT/'plugin/guest/task-main.py');consumer=importlib.util.module_from_spec(spec);spec.loader.exec_module(consumer)
class AcpLoggingTests(unittest.TestCase):
 def program(self):
  source=(ROOT/'plugin/guest/adapter.mjs').read_text();body=source.split('// BEGIN NATIVE ACP LOGGING:')[1].split('// END NATIVE ACP LOGGING')[0];body=body[body.index('function acpRedact'):]
  return "import fs from 'node:fs';import path from 'node:path';\nfunction read(p){return JSON.parse(fs.readFileSync(p,'utf8'))}\nfunction atomic(p,v){let tmp=p+'.tmp';fs.writeFileSync(tmp,JSON.stringify(v),{mode:0o600});fs.renameSync(tmp,p)}\n"+body
 def run_fixture(self,script):
  with tempfile.TemporaryDirectory()as d:
   file=pathlib.Path(d)/'fixture.mjs';file.write_text(self.program()+script);p=subprocess.run(['node',str(file),d],capture_output=True,text=True,timeout=10);self.assertEqual(p.returncode,0,p.stderr);return [json.loads(line)for line in(pathlib.Path(d)/'lifecycle.jsonl').read_text().splitlines()]
 def header(self):
  return "acpProcess=pid=>({pid:Number(pid),ticks:'fixture-start'});const dir=process.argv[2],session='"+str(uuid.uuid4())+"',request='"+str(uuid.uuid4())+"',record='"+str(uuid.uuid4())+"';const event=(n,message,type='message')=>({type,requestId:request,sequence:n,cursor:Buffer.from(JSON.stringify([record,n])).toString('base64url'),message,result:{stopReason:'end_turn'}});const update=u=>({method:'session/update',params:{sessionId:'native-session',update:u}});\n"
 def consume(self,rows):
  seen={};return [v for row in rows if(v:=consumer.lifecycle_event(row,seen))is not None]
 def test_actual_acpx_message_shape_messages_tools_end_and_split_secret_redaction(self):
  rows=self.run_fixture(self.header()+"""
 await nativeAcpLog(dir,session,request,record,event(1,update({sessionUpdate:'agent_message_chunk',content:{type:'text',text:'Hello. API_'}})));
 await nativeAcpLog(dir,session,request,record,event(2,update({sessionUpdate:'agent_message_chunk',content:{type:'text',text:'KEY=synthetic-secret\\n'}})));
 await nativeAcpLog(dir,session,request,record,event(3,update({sessionUpdate:'agent_thought_chunk',content:{type:'text',text:'private-reasoning-must-not-log'}})));
 await nativeAcpLog(dir,session,request,record,event(4,update({sessionUpdate:'tool_call',toolCallId:'tool-1',status:'in_progress',title:'Build wiki',rawInput:{env:{API_KEY:'raw-input-must-not-log'}}})));
 await nativeAcpLog(dir,session,request,record,event(5,update({sessionUpdate:'tool_call_update',toolCallId:'tool-1',status:'completed',rawOutput:'raw-output-must-not-log'})));
 await nativeAcpLog(dir,session,request,record,event(6,null,'turn_result'));
 """)
  out=self.consume(rows);text=''.join(r.get('text','')for r in out);self.assertIn('Hello.',text);self.assertIn('[REDACTED]',text);self.assertIn('Tool tool-1: in_progress',text);self.assertIn('completed',text);self.assertIn('ACP request ended',text)
  rendered=json.dumps(out);self.assertNotIn('synthetic-secret',rendered);self.assertNotIn('private-reasoning',rendered);self.assertNotIn('raw-input',rendered);self.assertNotIn('raw-output',rendered);self.assertTrue(all(r.get('session_id')and r.get('request_id')for r in out))
 def test_reconnect_and_crash_replay_no_duplicate_nomad_delivery(self):
  rows=self.run_fixture(self.header()+"""
 const e=event(1,update({sessionUpdate:'agent_message_chunk',content:{type:'text',text:'exactly-once-text\\n'}}));
 await nativeAcpLog(dir,session,request,record,e);await nativeAcpLog(dir,session,request,record,e);
 // Crash after append before state commit: replay creates identical sequence/part.
 fs.unlinkSync(path.join(dir,'native-log-'+request+'.json'));
 await nativeAcpLog(dir,session,request,record,e);
 await nativeAcpLog(dir,session,request,record,event(2,null,'turn_result'));
 """)
  self.assertEqual(''.join(r.get('text','')for r in self.consume(rows)).count('exactly-once-text'),1)
 def test_bounded_output_has_one_explicit_notice_and_terminal_even_after_limit(self):
  rows=self.run_fixture(self.header()+"""
 for(let n=1;n<=100;n++)await nativeAcpLog(dir,session,request,record,event(n,update({sessionUpdate:'agent_message_chunk',content:{type:'text',text:'x'.repeat(200)+'\\n'}})),4096);
 await nativeAcpLog(dir,session,request,record,event(101,null,'turn_result'),4096);
 """)
  self.assertLessEqual(sum(len((json.dumps(row,separators=(',',':'))+'\n').encode())for row in rows),4096);out=self.consume(rows);self.assertEqual(sum(r['stage']=='acp_log_truncated'for r in out),1);self.assertEqual(out[-1]['stage'],'acp_turn_end')
 def test_error_and_invalid_identity_are_not_raw_json_dump(self):
  rows=self.run_fixture(self.header()+"""
 await nativeAcpLog(dir,session,request,record,event(1,{error:{message:'Authorization: Bearer synthetic-secret'}}));
 await nativeAcpLog(dir,session,request,record,event(2,null,'turn_result'));
 """);out=self.consume(rows);self.assertEqual(out[0]['stage'],'acp_error');self.assertNotIn('synthetic-secret',json.dumps(out))
  bad={**rows[0],'request_id':None}
  with self.assertRaises(ValueError):consumer.lifecycle_event(bad,{})
 def test_quoted_headers_keys_and_multiline_chunk_boundaries(self):
  samples=[{'Authorization':'Bearer synthetic-review-secret'},{'Proxy-Authorization':'Basic synthetic-review-secret'},{'X-Vault-Token':'synthetic-review-secret'},{'X-Api-Key':'synthetic-review-secret'},{'apiKey':'synthetic-review-secret'},{'access_token':'synthetic-review-secret'},{'client_secret':'synthetic-review-secret'}]
  for sample in samples:
   value=json.dumps(sample,indent=2)
   # Every character is a separate ACP chunk, including JSON whitespace/newlines.
   script=self.header()+"const text="+json.dumps(value)+";let n=0;for(const ch of text)await nativeAcpLog(dir,session,request,record,event(++n,update({sessionUpdate:'agent_message_chunk',content:{type:'text',text:ch}})));await nativeAcpLog(dir,session,request,record,event(++n,null,'turn_result'));"
   rows=self.run_fixture(script);self.assertNotIn('synthetic-review-secret',json.dumps(rows));self.assertNotIn('synthetic-review-secret',json.dumps(self.consume(rows)))
   # Consumer independently masks producer-bypass rows, including quoted JSON.
   row={**rows[0],'text':value};self.assertNotIn('synthetic-review-secret',json.dumps(consumer.lifecycle_event(row,{})))
 def test_escaped_quoted_value_and_single_quote_header(self):
  for value in ['{"Authorization":"Bearer synthetic-review-secret', '{"Authorization":"Bearer synthetic-review-secret\\\"tail"}',"{'Authorization': 'Bearer synthetic-review-secret'}"]:
   rows=self.run_fixture(self.header()+"await nativeAcpLog(dir,session,request,record,event(1,{error:{message:"+json.dumps(value)+"}}));await nativeAcpLog(dir,session,request,record,event(2,null,'turn_result'));")
   self.assertNotIn('synthetic-review-secret',json.dumps(rows));self.assertNotIn('synthetic-review-secret',consumer.Redactor().text(value))
if __name__=='__main__':unittest.main()
