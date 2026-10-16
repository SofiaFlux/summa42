#!/usr/bin/env python3
import sys
import subprocess
import json
import re

def main():
    raw_input = sys.stdin.read().strip()
    if not raw_input:
        sys.exit(1)

    system_prompt = (
        "You are an automated, silent, non-interactive backend model for Summa42 orchestration runtime. "
        "Read the following JSON payload representing a task (classification, plan, review, implementation, or code review). "
        "Respond ONLY with a valid, strictly conforming single JSON object according to the requested schema. "
        "Do NOT include any conversational filler, explanations, markdown formatting, or code fences (no ```json). "
        "Output ONLY the raw JSON object and nothing else."
    )
    full_prompt = f"{system_prompt}\n\nINPUT PAYLOAD:\n{raw_input}"

    cmd = ["agy", "-p", full_prompt, "--dangerously-skip-permissions"]
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr)
        sys.exit(proc.returncode)

    stdout = proc.stdout.strip()
    match = re.search(r"(\{.*\})", stdout, re.DOTALL)
    if not match:
        sys.stderr.write(f"failed to find JSON in output: {stdout}\n")
        sys.exit(1)

    try:
        parsed = json.loads(match.group(1))
        sys.stdout.write(json.dumps(parsed))
    except Exception as e:
        sys.stderr.write(f"failed to parse extracted JSON: {e}\nRaw output: {stdout}\n")
        sys.exit(1)

if __name__ == "__main__":
    main()
