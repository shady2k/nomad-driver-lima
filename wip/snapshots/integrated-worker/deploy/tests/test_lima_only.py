"""Preparation-only transition checks; no host access or service actions."""
import copy
import importlib.util
import json
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('transition',ROOT/'deploy/prepare-lima-only.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class TransitionTests(unittest.TestCase):
    def setUp(self):
        self.source = {'data_dir':'/tmp/lima-only-fixture/data','plugin_dir':'/tmp/lima-only-fixture/plugins',
                       'client':{'enabled':True,'options':{'driver.allowlist':'boxlite', 'keep':'value'},
                                                'reserved':{'cpu':500,'memory':2048,'disk':10240}},
                       'plugin':[{'nomad-driver-boxlite':[{'config':[{'keep_old_data':True}]}]},
                                 {'unrelated-plugin':[{'config':[{'unchanged':True}]}]}]}
        self.lima = {'runtime_user':'runtime','limactl':'/opt/lima/bin/limactl','lima_home':'/var/lib/lima/home',
                     'state_dir':'/var/lib/lima/state','template_file':'/var/lib/lima/template.yaml',
                     'template_sha256':'a'*64,'expected_version':'2.2.1','memory_overhead_mib':128,
                     'disk_free_floor_mib':2048}

    def test_only_driver_changes_and_preserved_source(self):
        before = copy.deepcopy(self.source)
        result,plan=module.candidate(self.source,self.lima,'/var/lib/private-disabled')
        self.assertEqual(self.source,before)
        self.assertEqual(result['client']['options'],{'driver.allowlist':'lima','keep':'value'})
        self.assertEqual(result['client']['reserved'],before['client']['reserved'])
        self.assertEqual(result['plugin'][0],before['plugin'][1])
        self.assertEqual(result['plugin'][1],{'nomad-driver-lima':[{'config':[self.lima]}]})
        self.assertEqual(plan['removed_boxlite_stanzas'],1)
        self.assertFalse(plan['applied']);self.assertFalse(plan['restart_performed'])
        self.assertEqual(plan['discovery_move']['destination'],'/var/lib/private-disabled/nomad-driver-boxlite')
        self.assertEqual(result['plugin_dir'],before['plugin_dir'])

    def test_reject_unsafe_discovery_and_server_target(self):
        for disabled in ['/tmp/lima-only-fixture/plugins','/tmp/lima-only-fixture/plugins/disabled','/tmp/../plugins']:
            with self.assertRaises(ValueError):module.candidate(self.source,self.lima,disabled)
        self.source['server']={'enabled':True}
        with self.assertRaises(ValueError):module.candidate(self.source,self.lima,'/private/disabled')

    def test_catalog_requires_pinned_metadata_and_positive_budget_without_capacity_change(self):
        selected=dict(self.lima,catalog_file='/var/lib/lima/catalog.json',catalog_sha256='b'*64,max_total_disk_gib=80)
        result,_=module.candidate(self.source,selected,'/var/lib/private-disabled')
        self.assertEqual(result['client']['reserved'],self.source['client']['reserved'])
        self.assertEqual(result['plugin'][-1]['nomad-driver-lima'][0]['config'][0],selected)
        for overrides in ({'max_total_disk_gib':0},{'max_total_disk_gib':True},{'catalog_sha256':'not-pinned'},{'catalog_file':'relative.json'}):
            with self.assertRaises(ValueError):module.candidate(self.source,dict(selected,**overrides),'/var/lib/private-disabled')

    def test_private_cli_output_and_native_config_validation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp);source=root/'source.json';source.write_text(json.dumps(self.source))
            lima=root/'lima.json';lima.write_text(json.dumps(self.lima));before=source.read_bytes()
            subprocess.run(['python3',str(ROOT/'deploy/prepare-lima-only.py'),'--source-config',str(source),
                            '--lima-config',str(lima),'--disabled-plugin-dir','/var/lib/private-disabled',
                            '--output',str(root/'out')],check=True,capture_output=True)
            self.assertEqual(source.read_bytes(),before)
            self.assertEqual((root/'out').stat().st_mode & 0o777,0o700)
            candidate=root/'out/client.lima-only.json'
            self.assertEqual(candidate.stat().st_mode & 0o777,0o600)
            if shutil.which('nomad'):
                # Unrelated plugin is allowed in syntax-only full config validation.
                subprocess.run(['nomad','config','validate',str(candidate)],check=True,capture_output=True)

if __name__=='__main__':unittest.main()
