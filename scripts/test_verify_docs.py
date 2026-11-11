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

    def test_repeated_heading_gets_numbered_anchor(self):
        # GitHub numbers repeated headings (#decision, #decision-1, ...);
        # a repeated heading is valid and each occurrence can be linked.
        with tempfile.TemporaryDirectory() as tmp:
            doc = Path(tmp) / "a.md"
            body = "## Decision\n\n## Decision\n\n## Decision\n\n"
            doc.write_text(body + "[a](#decision) [b](#decision-1) [c](#decision-2)\n", encoding="utf-8")
            verify_docs.validate(doc)
            self.assertEqual(verify_docs.markdown_anchors(doc), {"decision", "decision-1", "decision-2"})
            doc.write_text(body + "[d](#decision-3)\n", encoding="utf-8")
            with self.assertRaises(verify_docs.ValidationError):
                verify_docs.validate(doc)

    def test_numbered_anchor_skips_a_taken_one(self):
        # A literal "Decision 1" heading already owns #decision-1, so the
        # repeated "Decision" takes the next free number, as GitHub does.
        with tempfile.TemporaryDirectory() as tmp:
            doc = Path(tmp) / "a.md"
            doc.write_text("## Decision 1\n\n## Decision\n\n## Decision\n", encoding="utf-8")
            self.assertEqual(verify_docs.markdown_anchors(doc), {"decision-1", "decision", "decision-2"})

    def test_file_url_is_not_a_valid_link(self):
        with tempfile.TemporaryDirectory() as tmp:
            doc = Path(tmp) / "a.md"
            (Path(tmp) / "b.html").write_text("x", encoding="utf-8")
            doc.write_text("[b](file:///somewhere/else/b.html)\n", encoding="utf-8")
            with self.assertRaises(verify_docs.ValidationError):
                verify_docs.validate(doc)
            doc.write_text("[b](b.html)\n", encoding="utf-8")
            verify_docs.validate(doc)


if __name__ == "__main__":
    unittest.main()
