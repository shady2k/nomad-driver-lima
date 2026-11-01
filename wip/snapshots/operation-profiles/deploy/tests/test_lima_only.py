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

    def test_secret_profile_install_projection_requires_exact_private_pin(self):
        selected=dict(self.lima,vault_address='https://secrets.example.invalid',secret_profiles_file='/var/lib/lima/state/secret-profiles.config',secret_profiles_sha256='d'*64,secret_profiles_bao_bin='/opt/bao')
        result,_=module.candidate(self.source,selected,'/var/lib/private-disabled')
        self.assertEqual(result['client']['reserved'],self.source['client']['reserved'])
        for field in ('secret_profiles_file','secret_profiles_sha256','secret_profiles_bao_bin','vault_address'):
            bad=dict(selected);bad.pop(field)
            with self.assertRaises(ValueError):module.candidate(self.source,bad,'/var/lib/private-disabled')
        for extra in ({'secret_profiles_file':'/var/lib/lima/state/profiles.json'},{'secret_profiles_bao_bin':'relative'},{'secret_profiles_sha256':'wrong'}):
            with self.assertRaises(ValueError):module.candidate(self.source,dict(selected,**extra),'/var/lib/private-disabled')

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

    def test_repository_profile_pair_pinned_and_other_configuration_preserved(self):
        selected=dict(self.lima,repository_profiles_file='/var/lib/lima/state/repository-profiles.config',repository_profiles_sha256='c'*64)
        result,_=module.candidate(self.source,selected,'/var/lib/private-disabled')
        self.assertEqual(result['client']['reserved'],self.source['client']['reserved'])
        self.assertEqual(result['plugin'][-1]['nomad-driver-lima'][0]['config'][0],selected)
        for overrides in ({'repository_profiles_file':'/var/lib/lima/state/profiles.json'},{'repository_profiles_sha256':'bad'},{'repository_profiles_file':'relative.json'},{'repository_profiles_file':'/private/../escape.json'}):
            with self.assertRaises(ValueError):module.candidate(self.source,dict(selected,**overrides),'/var/lib/private-disabled')
        for missing in ('repository_profiles_file','repository_profiles_sha256'):
            partial=dict(selected);partial.pop(missing)
            with self.assertRaises(ValueError):module.candidate(self.source,partial,'/var/lib/private-disabled')

    def test_repository_filter_validates_actual_guest_schema_without_connecting(self):
        path=ROOT/'deploy/roles/lima/filter_plugins/lima_transition.py'
        spec=importlib.util.spec_from_file_location('profile_filter',path);filters=importlib.util.module_from_spec(spec);spec.loader.exec_module(filters)
        profile={'schema_version':1,'id':'cache','version':'v1','npm':{'registry':'https://cache.example.invalid/npm/'}}
        cfg={'repository_profile_default':'cache','repository_profiles':{'cache':profile}}
        self.assertEqual(filters.repository_profiles(cfg),{'valid':True,'profiles':['cache'],'default':'cache'})
        for bad in (dict(cfg,repository_profile_default='missing'),dict(cfg,credentials='not-allowed'),{'repository_profiles':{'other':profile}}):
            with self.assertRaises((ValueError,RuntimeError)):filters.repository_profiles(bad)
        cfg['repository_profiles']['cache']['npm']['registry']='https://user:password@cache.example.invalid/'
        with self.assertRaises((ValueError,RuntimeError)):filters.repository_profiles(cfg)

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
