import importlib.util,json,pathlib,sys,tempfile,time,unittest,uuid,os
from unittest import mock
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'scripts'))
import operations
import test_operations as op_fixture
ROOT=pathlib.Path(__file__).parents[2]
def load(name,path):
 spec=importlib.util.spec_from_file_location(name,path);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m
consumer=load('accepted_v5_consumer',ROOT/'plugin/guest/task-main.py');producer=load('progress_producer',ROOT/'plugin/scripts/operation_guest.py')
class ProgressTests(unittest.TestCase):
 def test_rendered_producer_to_existing_consumer_has_steps_bytes_heartbeat_redaction(self):
  with tempfile.TemporaryDirectory()as d:
   fixture=op_fixture.OperationsTests();a,s,gs,calls,code=fixture.fixture(d);op=str(uuid.uuid4());code=code.replace('max(30,','max(0.2,') # fixture only; policy=1s, production minimum30s
   script='import time;print("@@worker-step-start download-go",flush=True);print("@@worker-download 65536 unknown",flush=True);print("API_KEY=synthetic-secret",flush=True);time.sleep(1.3);print("@@worker-step-end download-go 0",flush=True)'
   with mock.patch.object(operations,'program',return_value=code):
    operations.start(a,s,op,[sys.executable,'-c',script],'prepare-tools');self.assertEqual(fixture.done(a,s,op)['phase'],'completed')
   rows=[consumer.lifecycle_event(json.loads(line))for line in(gs/'lifecycle.jsonl').read_text().splitlines()];rendered=''.join(row.get('text','')for row in rows)
   if os.environ.get('NOMAD_PROGRESS_FIXTURE_DIR'):
    dest=pathlib.Path(os.environ['NOMAD_PROGRESS_FIXTURE_DIR']);dest.mkdir(parents=True,exist_ok=True);(dest/'nomad-v5.jsonl').write_text(''.join(json.dumps(row)+'\n'for row in rows));(dest/'nomad-readable.txt').write_text(rendered)
   self.assertIn('Step download-go started.',rendered);self.assertIn('65536 bytes / unknown total',rendered);self.assertIn('Step download-go finished: exit=0, elapsed=',rendered);self.assertIn('Still running; step=download-go;',rendered);self.assertIn('no complete output for',rendered);self.assertNotIn('synthetic-secret',rendered);self.assertIn('[REDACTED]',rendered);self.assertTrue(all(row.get('stream')in('stdout','stderr')for row in rows if 'text'in row))
 def test_byte_progress_is_reported_not_inferred_and_throttled(self):
  s={'label':'install-go','_monotonic':0};producer.progress_line(s,'@@worker-step-start download-go\n',0);text,phase,emit=producer.progress_line(s,'@@worker-download 1024 4096\n',1);self.assertTrue(emit);self.assertIn('1024 bytes / 4096 bytes',text)
  self.assertFalse(producer.progress_line(s,'@@worker-download 1024 4096\n',20)[2]);self.assertIn('REJECTED',producer.progress_line(s,'@@worker-download 1 4096\n',21)[0]);self.assertNotIn('%',producer.quiet_text(s,30,1));self.assertNotIn('bytes',producer.quiet_text(s,30,1))
 def test_carriage_return_download_meter_is_incremental_not_buffered_until_end(self):
  with tempfile.TemporaryDirectory()as d:
   fixture=op_fixture.OperationsTests();a,s,gs,calls,code=fixture.fixture(d);op=str(uuid.uuid4());script='import sys,time;sys.stderr.write("download 10MiB\\r");sys.stderr.flush();time.sleep(.5);sys.stderr.write("download 20MiB\\r");sys.stderr.flush()'
   with mock.patch.object(operations,'program',return_value=code):operations.start(a,s,op,[sys.executable,'-c',script],'download-go');fixture.done(a,s,op)
   text=''.join(consumer.lifecycle_event(json.loads(line)).get('text','')for line in(gs/'lifecycle.jsonl').read_text().splitlines());self.assertIn('download 10MiB\n',text);self.assertIn('download 20MiB\n',text)

 def test_supported_shell_helper_runs_inside_owned_fixture_and_renders_actual_bytes(self):
  with tempfile.TemporaryDirectory()as d:
   fixture=op_fixture.OperationsTests();a,s,gs,calls,code=fixture.fixture(d);op=str(uuid.uuid4());helper=(ROOT/'plugin/examples/managed-recipe.sh').read_text()
   # Fake curl only writes four local fixture bytes. No network or package install.
   script=helper+"\nstat() { "+__import__('shlex').quote(sys.executable)+" -c \"import pathlib,sys; print(pathlib.Path(sys.argv[1]).stat().st_size)\" \"${@: -1}\"; }\nsleep() { command sleep 0.05; }\ncurl() { local dest; while (($#)); do if [[ \"$1\" == --output ]]; then dest=\"$2\"; shift 2; else shift; fi; done; printf abcd > \"$dest\"; command sleep 0.1; return 0; }\nworker_download download-fixture https://fixture.invalid "+__import__('shlex').quote(str(pathlib.Path(d)/'payload'))+"\n"
   with mock.patch.object(operations,'program',return_value=code):operations.start(a,s,op,['/bin/bash','-c',script],'prepare-tools');row=fixture.done(a,s,op)
   self.assertEqual(row['phase'],'completed');text=''.join(consumer.lifecycle_event(json.loads(v)).get('text','')for v in(gs/'lifecycle.jsonl').read_text().splitlines());self.assertIn('Step download-fixture started.',text);self.assertIn('4 bytes / unknown total (final byte count; see step result)',text);self.assertIn('Step download-fixture finished: exit=0',text)
