import tempfile
import unittest
from pathlib import Path

import verify_docs


class VerifyDocsTests(unittest.TestCase):
    def test_underscore_anchor(self):
        self.assertEqual(verify_docs.github_anchor("My_Heading (v2)"), "my_heading-v2")
        with tempfile.TemporaryDirectory() as tmp:
            doc = Path(tmp) / "a.md"
            doc.write_text("# snake_case\n\n[x](#snake_case)\n", encoding="utf-8")
            verify_docs.validate(doc)

    def test_root_relative_link(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / ".git").mkdir()
            (root / "docs").mkdir()
            (root / "docs" / "x.md").write_text("# X\n", encoding="utf-8")
            (root / "sub").mkdir()
            doc = root / "sub" / "a.md"
            doc.write_text("[x](/docs/x.md)\n", encoding="utf-8")
            verify_docs.validate(doc)
            doc.write_text("[x](/docs/missing.md)\n", encoding="utf-8")
            with self.assertRaises(verify_docs.ValidationError):
                verify_docs.validate(doc)


if __name__ == "__main__":
    unittest.main()
