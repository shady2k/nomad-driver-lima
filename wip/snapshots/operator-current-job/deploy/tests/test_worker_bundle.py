"""Offline deployment integration checks. No VMs, network, daemon or model call."""
import base64
import hashlib
import json
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
NODE = '00000000-0000-4000-8000-000000000001'
OWNER = '00000000-0000-4000-8000-000000000002'

class BundleTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.out = pathlib.Path(self.tmp.name)/'bundle'
        self.cmd = ['python3', str(ROOT/'deploy/prepare-worker.py'), '--output',str(self.out),
                    '--node-id',NODE,'--owner-id',OWNER,'--namespace','fixture',
                    '--datacenter','fixture','--cpu-mhz','1200','--provider','external-fixture',
                    '--model','fixture-model','--output-token-budget','100',
                    '--receipt-root','/private/external/results']
        subprocess.run(self.cmd,check=True,capture_output=True)

    def test_native_nomad_parse_and_plugin_target_contract(self):
        if not shutil.which('nomad'): self.skipTest('Nomad CLI unavailable')
        p = subprocess.run(['nomad','job','run','-output','-var-file='+str(self.out/'worker.vars.hcl'),
                            str(self.out/'worker.nomad.hcl')],check=True,capture_output=True,text=True)
        j = json.loads(p.stdout)['Job']
        self.assertEqual(j['ID'],'lima-worker-canary')
        self.assertEqual(j['Namespace'],'fixture')
        self.assertEqual(j['Meta']['nomad_workers_owner'],OWNER)
        self.assertTrue(any(c['LTarget']=='${node.unique.id}' and c['RTarget']==NODE for c in j['Constraints']))
        g = j['TaskGroups'][0]; t = g['Tasks'][0]
        self.assertEqual((g['Count'],t['Name'],t['Driver']),(1,'worker','lima'))
        self.assertEqual(g['RestartPolicy']['Attempts'],0)
        self.assertEqual(g['ReschedulePolicy']['Attempts'],0)
        self.assertFalse(g['ReschedulePolicy']['Unlimited'])
        self.assertEqual(t['Resources']['MemoryMB'],4224)
        self.assertEqual(t['Config']['architecture'],'amd64')
        self.assertEqual(t['Config']['command'],['/bin/sh','/opt/nomad-workers/start-worker.sh'])
        c = json.loads((self.out/'coordinator.json').read_text())
        self.assertEqual(c['owner_id'],j['Meta']['nomad_workers_owner'])
        self.assertEqual(c['job'],j['ID'])
        self.assertEqual(c['task'],t['Name'])
        self.assertEqual(c['guest']['agent_argv'][0],'/usr/bin/env')
        self.assertFalse(c['guest']['runtime_contract']['effective_provider_config_verified'])

    def test_secret_free_template_payload_and_shell_syntax(self):
        template = json.loads((self.out/'worker-template.yaml').read_text())
        self.assertTrue(template['plain']); self.assertEqual(template['mounts'],[])
        self.assertFalse(template['ssh']['forwardAgent'])
        script = template['provision'][0]['script']
        subprocess.run(['/bin/sh','-n'],input=script,text=True,check=True)
        subprocess.run(['/bin/sh','-n',str(ROOT/'deploy/guest/start-worker.sh')],check=True)
        payload = script.split("base64.b64decode('",1)[1].split("')",1)[0]
        files = json.loads(base64.b64decode(payload))
        for name in ('adapter.mjs','launcher.mjs'):
            self.assertEqual(base64.b64decode(files['/opt/nomad-workers/'+name][0]),
                             (ROOT/'plugin/guest'/name).read_bytes())
        lock = json.loads(base64.b64decode(files['/opt/worker/acpx/package-lock.json'][0]))
        self.assertEqual(lock['packages']['node_modules/acpx']['version'],'0.21.0')
        self.assertNotIn('external-fixture',script); self.assertNotIn('fixture-model',script)
        self.assertNotIn('/var/lib/nomad-workers/provider/models.json',files)
        self.assertNotIn('systemctl',script)

    def test_manifest_and_no_overwrite(self):
        manifest=json.loads((self.out/'manifest.json').read_text())
        for name,digest in manifest['files'].items():
            self.assertEqual(hashlib.sha256((self.out/name).read_bytes()).hexdigest(),digest)
        self.assertFalse(manifest['applied']); self.assertEqual(manifest['model_calls'],0)
        before=(self.out/'manifest.json').read_bytes()
        p=subprocess.run(self.cmd,capture_output=True)
        self.assertNotEqual(p.returncode,0)
        self.assertEqual((self.out/'manifest.json').read_bytes(),before)

if __name__=='__main__': unittest.main()
