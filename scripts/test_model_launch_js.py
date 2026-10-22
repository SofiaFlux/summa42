import os
import shutil
import subprocess
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


@unittest.skipUnless(shutil.which("node"), "node not installed")
class ModelLaunchJs(unittest.TestCase):
    def test_node_suite(self):
        res = subprocess.run(["node", "--test", os.path.join(HERE, "model-launch.test.js")],
                             capture_output=True, text=True, timeout=120)
        self.assertEqual(res.returncode, 0, res.stdout + res.stderr)


if __name__ == "__main__":
    unittest.main()
