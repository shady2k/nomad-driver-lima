import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('publication_licenses', Path(__file__).with_name('licenses.py'))
licenses = importlib.util.module_from_spec(spec)
spec.loader.exec_module(licenses)

class DependencyNoticeTests(unittest.TestCase):
    def test_case_sensitive_notice_names_and_nested_scope_are_preserved(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            module = root / 'module'
            package = module / 'sdk'
            package.mkdir(parents=True)
            outer = b'Fixture module-wide license parameters\n'
            inner = b'Fixture nested source license\n'
            (module / 'LICENSE').write_bytes(outer)
            (package / 'License').write_bytes(inner)
            (package / 'types.go').write_text('// SPDX-License-Identifier: MPL-2.0\npackage sdk\n')
            metadata = root / 'input.json'
            metadata.write_text(json.dumps({'ImportPath': 'example.invalid/module/sdk', 'Dir': str(package), 'Module': {'Path': 'example.invalid/module', 'Version': 'v1.0.0', 'Dir': str(module)}, 'GoFiles': ['types.go']}))
            output = root / 'output'
            with patch('sys.argv', ['licenses', str(metadata), str(output)]), contextlib.redirect_stdout(io.StringIO()):
                licenses.main()
            inventory = json.loads((output / 'inventory.json').read_text())
            self.assertFalse(inventory['missing_notice_packages'])
            self.assertEqual((output / 'licenses/example.invalid/module@v1.0.0/LICENSE').read_bytes(), outer)
            self.assertEqual((output / 'licenses/example.invalid/module@v1.0.0/sdk/License').read_bytes(), inner)
            self.assertEqual(inventory['packages'][0]['files'][0]['spdx_header'], 'MPL-2.0')
            self.assertNotIn(str(root), (output / 'inventory.json').read_text())

if __name__ == '__main__':
    unittest.main()
