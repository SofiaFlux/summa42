#!/usr/bin/env python3
"""
Backward-compatible wrapper for agy model adapter.
Delegates to the universal call-worker router.
"""
import sys
import os
import subprocess

cmd = [sys.executable, os.path.join(os.path.dirname(__file__), "call-worker.py"), "--backend", "agy"] + sys.argv[1:]
res = subprocess.run(cmd)
sys.exit(res.returncode)
