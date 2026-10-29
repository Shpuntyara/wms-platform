import hashlib
import json
from pathlib import Path
import subprocess
import unittest


class WorkplaceReceiptTests(unittest.TestCase):
    def test_git_preserves_the_exact_recorded_execution_bytes(self):
        root = Path(__file__).resolve().parents[1]
        base = root / 'docs/verification/workplace'
        report = json.loads((base / 'receipts.json').read_text(encoding='utf8'))
        self.assertGreaterEqual(len(report['receipt_sha256']), 60)
        for name, expected in report['receipt_sha256'].items():
            with self.subTest(receipt=name):
                relative = (base / name).relative_to(root).as_posix()
                actual = subprocess.check_output(['git', 'show', ':' + relative], cwd=root)
                self.assertEqual(hashlib.sha256(actual).hexdigest(), expected)


if __name__ == '__main__':
    unittest.main()
