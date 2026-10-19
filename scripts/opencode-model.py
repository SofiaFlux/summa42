#!/usr/bin/env python3
"""
Backward-compatible wrapper for opencode model adapter.
Delegates to the universal call-worker router.
"""
import sys
import os
import subprocess

cmd = [sys.executable, os.path.join(os.path.dirname(__file__), "call-worker.py"), "--backend", "opencode"] + sys.argv[1:]
res = subprocess.run(cmd)
sys.exit(res.returncode)
