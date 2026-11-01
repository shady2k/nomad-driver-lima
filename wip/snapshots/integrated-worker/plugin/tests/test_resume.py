import copy
import json
import pathlib
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
import uuid

sys.path.insert(0, str(pathlib.Path(__file__).parents[1] / 'scripts'))
import catalog_cli
import resume


class ResumeTests(unittest.TestCase):
    def fixture(self):
        config = {'namespace': 'workers', 'owner_id': str(uuid.uuid4()),
                  'agent_instance_id': str(uuid.uuid4()), 'task': 'worker',
                  'datacenters': ['dc'], 'vault': {'role': 'generic-runtime'},
                  'guest_bootstrap_command': ['/runtime', '--session={session_id}']}
        source = {'ID': str(uuid.uuid4()), 'Namespace': 'workers', 'NodeID': 'node-a',
                  'DesiredStatus': 'stop', 'ClientStatus': 'complete',
                  'Job': {'Meta': {'nomad_workers_owner': config['owner_id']},
                          'TaskGroups': [{'Tasks': [{'Name': 'worker', 'Driver': 'lima',
                              'Config': {'worker_session_id': str(uuid.uuid4()),
                                         'command': ['old-prompt-must-not-replay'],
                                         'guest_cpus': 2, 'memory_mib': 1024, 'disk_gib': 20,
                                         'architecture': 'arm64', 'guest_os': 'linux'},
                              'Resources': {'CPU': 2000, 'MemoryMB': 1152}}]}]},
                  'TaskStates': {'worker': {'Events': [{'DriverMessage': 'lima vm_stopped',
                    'Details': {'resume_source_key': 'a' * 64, 'resume_generation': '2',
                                'resume_node_id': 'node-a', 'stop_revocation_confirmed': 'true'}}]}}}
        return config, source, str(uuid.uuid4())

    def test_repository_profile_pin_survives_resume_plan(self):
        config, source, session = self.fixture()
        old = source['Job']['TaskGroups'][0]['Tasks'][0]['Config']
        old.update(repository_profile='cache', repository_profile_sha256='b'*64)
        task = resume.plan(config, source, session)['TaskGroups'][0]['Tasks'][0]
        self.assertEqual(task['Config']['repository_profile'], 'cache')
        self.assertEqual(task['Config']['repository_profile_sha256'], 'b'*64)

    def test_same_node_new_session_file_token_without_prompt_replay(self):
        config, source, session = self.fixture()
        original = copy.deepcopy(source)
        job = resume.plan(config, source, session)
        self.assertEqual(source, original)
        self.assertEqual(job['Constraints'], [{'LTarget': '${node.unique.id}', 'Operand': '=', 'RTarget': 'node-a'}])
        task = job['TaskGroups'][0]['Tasks'][0]
        self.assertEqual(task['Config']['resume_generation'], 2)
        self.assertEqual(task['Config']['worker_session_id'], session)
        self.assertEqual(task['Config']['command'], ['/runtime', '--session=' + session])
        self.assertNotIn('old-prompt', json.dumps(job))
        self.assertEqual(task['Vault'], {'Role': 'generic-runtime', 'Env': False, 'DisableFile': False, 'ChangeMode': 'noop'})
        self.assertEqual(task['Resources'], {'CPU': 2000, 'MemoryMB': 1152})
        self.assertEqual(job['TaskGroups'][0]['ReschedulePolicy']['Attempts'], 0)

    def test_source_must_be_owned_terminal_with_confirmed_receipt(self):
        for kind in ('owner', 'namespace', 'active', 'node', 'receipt', 'generation'):
            with self.subTest(kind=kind):
                config, source, session = self.fixture()
                receipt = source['TaskStates']['worker']['Events'][0]['Details']
                if kind == 'owner': config['owner_id'] = 'other'
                elif kind == 'namespace': config['namespace'] = 'other'
                elif kind == 'active': source['ClientStatus'] = 'running'
                elif kind == 'node': receipt['resume_node_id'] = 'other'
                elif kind == 'receipt': receipt['stop_revocation_confirmed'] = 'false'
                elif kind == 'generation': receipt['resume_generation'] = '0'
                with self.assertRaises(ValueError): resume.plan(config, source, session)

    def test_unknown_submit_is_lookup_only_and_cannot_change_source(self):
        config, source, session = self.fixture()
        with tempfile.TemporaryDirectory() as tmp:
            config['receipt_root'] = tmp
            path = pathlib.Path(tmp) / 'config'
            path.write_text(json.dumps(config))
            client = catalog_cli.CatalogClient(path)
            creator=pathlib.Path(tmp)/'creator.json'
            creator.write_text(json.dumps({'agent_instance_id':config['agent_instance_id'],'native_session_id':'new-creator','generation':str(uuid.uuid4()),'evidence':'native_hook_observation_not_authenticated_liveness','ended':False}))
            creator.chmod(0o600)
            client.api = lambda _: source
            calls = []
            def run(args, input=None):
                calls.append((args, input))
                return type('Result', (), {'returncode': 1})()
            client.run = run
            client.request_status = lambda _: {'lookup_only': True}
            with self.assertRaisesRegex(RuntimeError, 'UNKNOWN'): client.resume(session, source['ID'],creator)
            self.assertEqual(client.resume(session, source['ID'],creator), {'lookup_only': True})
            self.assertEqual(len(calls), 1)
            with self.assertRaisesRegex(ValueError, 'CHANGED'): client.resume(session, str(uuid.uuid4()),creator)

    def test_resume_binds_fresh_native_initiator_and_rejects_ended_record(self):
        config, source, session = self.fixture()
        with tempfile.TemporaryDirectory() as tmp:
            config.update(receipt_root=tmp,version=1)
            path=pathlib.Path(tmp)/'config';path.write_text(json.dumps(config))
            creator=pathlib.Path(tmp)/'creator';native={'agent_instance_id':str(uuid.uuid4()),'native_session_id':'new-native-session','generation':str(uuid.uuid4()),'evidence':'native_hook_observation_not_authenticated_liveness','ended':True}
            creator.write_text(json.dumps(native));creator.chmod(0o600)
            client=catalog_cli.CatalogClient(path);client.api=lambda _:source
            jobs=[]
            def run(args,input=None):
                jobs.append(json.loads(input)['Job'])
                return type('Result',(),{'returncode':0})()
            client.run=run;client.request_status=lambda _: {'lookup_only':True}
            with self.assertRaisesRegex(ValueError,'NOT_ACTIVE'):client.resume(session,source['ID'],creator)
            self.assertFalse(jobs)
            native['ended']=False;creator.write_text(json.dumps(native))
            client.resume(session,source['ID'],creator)
            intent=json.loads((pathlib.Path(tmp)/session/'allocation-intent.json').read_text())
            self.assertEqual(intent['creator_generation'],native['generation'])
            self.assertEqual(intent['creator_record'],str(creator))
            self.assertEqual(jobs[0]['Meta']['creator_agent_id'],native['agent_instance_id'])
            self.assertEqual(jobs[0]['TaskGroups'][0]['Tasks'][0]['Config']['creator_session_id'],'new-native-session')
            import workerctl
            binding={**config,'allocation':str(uuid.uuid4()),'agent_instance_id':native['agent_instance_id'],'creator_session_id':native['native_session_id'],'creator_generation':native['generation'],'creator_record':str(creator)}
            bound=pathlib.Path(tmp)/'bound';bound.write_text(json.dumps(binding));adapter=workerctl.Adapter(bound)
            workerctl.atomic(adapter.local(session)/'session.json',{'config_hash':adapter.hash,'phase':'ready'})
            native['ended']=True;creator.write_text(json.dumps(native))
            with self.assertRaisesRegex(RuntimeError,'INITIATOR_GENERATION'):adapter.submit(session,str(uuid.uuid4()),'must not dispatch')

    @unittest.skipUnless(shutil.which('node'),'Node required for actual guest adapter fixture')
    def test_guest_create_uses_new_acp_record_and_never_loads_old_prompt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);trace=root/'trace.jsonl';fake=root/'acpx'
            fake.write_text('#!'+sys.executable+'\n'+
                'import json,sys,uuid\n'+
                'from pathlib import Path\n'+
                'args=sys.argv[1:]\n'+
                'if args==["--version"]: print("0.21.0");sys.exit(0)\n'+
                'with open('+repr(str(trace))+',"a") as f:f.write(json.dumps(args)+"\\n")\n'+
                'assert "sessions" in args and "new" in args\n'+
                'print(json.dumps({"acpxRecordId":"fixture-"+str(uuid.uuid4())}))\n')
            fake.chmod(0o700)
            guest=pathlib.Path(__file__).parents[1]/'guest'
            config={'protocol_version':1,'state_dir':str(root/'state'),'workspace':str(root),
                'agent_argv':['/bin/true'],'node':shutil.which('node'),'launcher':str(guest/'launcher.mjs'),
                'acpx_bin':str(fake),'acpx_version':'0.21.0','runtime_module':str(root/'unused-runtime.js'),
                'max_artifact_bytes':1024}
            def create(session):
                result=subprocess.run([shutil.which('node'),str(guest/'adapter.mjs')],input=json.dumps({'action':'create','session_id':session,'config':config}),text=True,capture_output=True,timeout=10)
                self.assertEqual(result.returncode,0,result.stdout+result.stderr)
                return json.loads(result.stdout.splitlines()[-1])
            old,new=str(uuid.uuid4()),str(uuid.uuid4())
            before=create(old)
            oldturn=root/'state'/old/'turn-old.json';oldturn.write_text(json.dumps({'prompt':'never replay this old prompt','phase':'terminal'}))
            after=create(new)
            self.assertNotEqual(before['record_id'],after['record_id'])
            self.assertEqual(after['session_id'],new)
            self.assertEqual(create(new)['record_id'],after['record_id'])
            calls=[json.loads(line) for line in trace.read_text().splitlines()]
            self.assertEqual(len(calls),2)
            self.assertEqual([row[row.index('--name')+1] for row in calls],[old,new])
            self.assertTrue(all('prompt' not in row and 'load' not in row for row in calls))
            self.assertFalse(list((root/'state'/new).glob('turn-*')))
            self.assertIn('never replay',oldturn.read_text())


if __name__ == '__main__': unittest.main()
