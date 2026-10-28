import hashlib
from pathlib import Path
import os
import tempfile
import unittest
from unittest.mock import patch

from collect_resilience_evidence import collect


class CollectEvidenceTests(unittest.TestCase):
    def test_private_pgdata_is_never_scanned_and_selected_bytes_are_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'run/source/pgdata').mkdir(parents=True)
            (root / 'run/source/pgdata/private.jsonl').write_text('do not publish')
            (root / 'final.json').write_text('{"status":"failed","error":"synthetic interrupted run"}')
            (root / 'run/source/postgres.log').write_text('redo starts at')
            dump = root / 'run/synthetic-wms.dump'
            dump.write_bytes(b'PGDMP synthetic bytes')
            original = os.scandir
            def guarded(path):
                if Path(path).name == 'pgdata':
                    raise PermissionError('PGDATA must remain private')
                return original(path)
            with patch('collect_resilience_evidence.os.scandir', side_effect=guarded):
                manifest = collect(root, root / 'published')
            self.assertEqual(set(manifest), {'final.json', 'run/source/postgres.log', 'run/synthetic-wms.dump'})
            self.assertEqual((root / 'published/run/synthetic-wms.dump').read_bytes(), dump.read_bytes())
            self.assertEqual(manifest['run/synthetic-wms.dump']['sha256'], hashlib.sha256(dump.read_bytes()).hexdigest())
            self.assertFalse((root / 'published/run/source/pgdata').exists())

    def test_destination_cannot_overwrite_the_source_or_escape_its_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for output in (root, root.parent / 'outside-evidence'):
                with self.subTest(output=output), self.assertRaises(ValueError):
                    collect(root, output)


if __name__ == '__main__':
    unittest.main()
