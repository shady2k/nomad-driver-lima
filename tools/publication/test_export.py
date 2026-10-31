import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('publication_export', Path(__file__).with_name('export.py'))
export = importlib.util.module_from_spec(spec)
spec.loader.exec_module(export)

class ExportBoundaryTests(unittest.TestCase):
    def run_export(self, entries, include, exclude=()):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        (root / 'manifest.json').write_text(json.dumps({'include': include, 'exclude': list(exclude)}))
        data = io.BytesIO()
        with tarfile.open(fileobj=data, mode='w') as archive:
            for name, content, kind in entries:
                member = tarfile.TarInfo(name)
                member.type = kind
                if kind == tarfile.SYMTYPE:
                    member.linkname = '/outside/private'
                else:
                    member.size = len(content)
                archive.addfile(member, io.BytesIO(content) if kind == tarfile.REGTYPE else None)
        output = root / 'snapshot'
        with patch.object(export, 'HERE', root), patch.object(export.subprocess, 'check_output', side_effect=['0' * 40 + '\n', data.getvalue()]), patch('sys.argv', ['export', '--ref', 'HEAD', '--output', str(output)]), contextlib.redirect_stdout(io.StringIO()):
            try:
                export.main()
            except SystemExit:
                self.assertFalse(output.exists(), 'Invalid source must fail before writing any public files')
                raise
        return output

    def test_unreviewed_file_blocks_export_before_creation(self):
        with self.assertRaises(SystemExit):
            self.run_export([('private.json', b'fixture', tarfile.REGTYPE)], [])

    def test_symlink_blocks_export_before_creation(self):
        with self.assertRaises(SystemExit):
            self.run_export([('public.txt', b'', tarfile.SYMTYPE)], ['public.txt'])

    def test_path_traversal_blocks_export_before_creation(self):
        with self.assertRaises(SystemExit):
            self.run_export([('../outside', b'fixture', tarfile.REGTYPE)], ['../outside'])

    def test_only_reviewed_public_bytes_are_exported(self):
        out = self.run_export([('public.txt', b'public fixture', tarfile.REGTYPE), ('private.json', b'private fixture', tarfile.REGTYPE)], ['public.txt'], ['private.json'])
        self.assertEqual((out / 'public.txt').read_bytes(), b'public fixture')
        self.assertFalse((out / 'private.json').exists())
        self.assertFalse((out / '.git').exists())
        self.assertTrue(out.with_suffix('.provenance.json').is_file())

if __name__ == '__main__':
    unittest.main()
