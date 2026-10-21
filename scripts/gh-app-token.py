#!/usr/bin/env python3
"""
Generate a short-lived GitHub App Installation Access Token for Summa42.
Saves the token to ~/.summa42/github_token (mode 0600) and prints it.
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.request
import jwt

CONFIG_DIR = os.path.expanduser("~/.summa42")
APP_JSON_PATH = os.path.join(CONFIG_DIR, "gh-app.json")
TOKEN_FILE_PATH = os.path.join(CONFIG_DIR, "github_token")
EXPECTED_ACCOUNT = "sofiaflux"

def generate_jwt(app_id: int, private_key_pem: str) -> str:
    now = int(time.time())
    payload = {
        "iat": now - 60,
        "exp": now + (10 * 60),
        "iss": str(app_id)
    }
    encoded = jwt.encode(payload, private_key_pem, algorithm="RS256")
    if isinstance(encoded, bytes):
        encoded = encoded.decode("utf-8")
    return encoded

def get_installations(app_jwt: str):
    req = urllib.request.Request(
        "https://api.github.com/app/installations",
        headers={
            "Authorization": f"Bearer {app_jwt}",
            "Accept": "application/vnd.github+json",
            "User-Agent": "summa42-token-generator"
        }
    )
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read().decode("utf-8"))

def get_installation_token(installation_id: int, app_jwt: str) -> dict:
    req = urllib.request.Request(
        f"https://api.github.com/app/installations/{installation_id}/access_tokens",
        data=b"{}",
        headers={
            "Authorization": f"Bearer {app_jwt}",
            "Accept": "application/vnd.github+json",
            "User-Agent": "summa42-token-generator"
        },
        method="POST"
    )
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read().decode("utf-8"))

def parse_age(spec: str) -> int:
    m = re.fullmatch(r"(\d+)([smh])", spec)
    if not m:
        raise argparse.ArgumentTypeError(f"invalid duration {spec!r}; use e.g. 50m, 3600s, 1h")
    return int(m.group(1)) * {"s": 1, "m": 60, "h": 3600}[m.group(2)]

def main():
    parser = argparse.ArgumentParser(description="Generate GitHub App installation token for Summa42")
    parser.add_argument("--print-token", "-p", action="store_true", help="Print token to stdout")
    parser.add_argument("--refresh-if-older-than", type=parse_age, default=None, metavar="DURATION",
                        help="Do nothing if the token file is younger than DURATION (e.g. 50m); for timers")
    args = parser.parse_args()

    if args.refresh_if_older_than is not None and os.path.exists(TOKEN_FILE_PATH):
        age = time.time() - os.path.getmtime(TOKEN_FILE_PATH)
        if age < args.refresh_if_older_than:
            print(f"Token file is {int(age)}s old (< {args.refresh_if_older_than}s); not refreshing.")
            if args.print_token:
                with open(TOKEN_FILE_PATH, "r") as f:
                    print(f.read().strip())
            return

    if not os.path.exists(APP_JSON_PATH):
        print(f"Error: {APP_JSON_PATH} not found. Please run scripts/register-gh-app.py first.", file=sys.stderr)
        sys.exit(1)

    with open(APP_JSON_PATH, "r") as f:
        meta = json.load(f)

    app_id = meta.get("app_id")
    pem_path = meta.get("pem_path", os.path.join(CONFIG_DIR, "gh-app.pem"))

    if not os.path.exists(pem_path):
        print(f"Error: PEM key not found at {pem_path}", file=sys.stderr)
        sys.exit(1)

    with open(pem_path, "r") as f:
        pem_key = f.read()

    app_jwt = generate_jwt(app_id, pem_key)

    installation_id = meta.get("installation_id")
    if not installation_id:
        # Discover installation
        installations = get_installations(app_jwt)
        if not installations:
            slug = meta.get("slug", "summa42-bot")
            print(f"Error: No installations found for App '{slug}'.", file=sys.stderr)
            print(f"Please install the App on your repository:", file=sys.stderr)
            print(f"  https://github.com/apps/{slug}/installations/new", file=sys.stderr)
            sys.exit(1)

        # Only the expected account is acceptable; never fall back to an arbitrary installation.
        target_inst = None
        for inst in installations:
            account = inst.get("account", {}).get("login", "")
            if account.lower() == EXPECTED_ACCOUNT:
                target_inst = inst
                break
        if not target_inst:
            accounts = [inst.get("account", {}).get("login", "?") for inst in installations]
            print(f"Error: No installation of this App on account '{EXPECTED_ACCOUNT}'. "
                  f"Found: {', '.join(accounts)}.", file=sys.stderr)
            sys.exit(1)

        installation_id = target_inst["id"]
        meta["installation_id"] = installation_id
        meta["installation_account"] = target_inst.get("account", {}).get("login")
        with open(APP_JSON_PATH, "w") as f:
            json.dump(meta, f, indent=2)

    token_data = get_installation_token(installation_id, app_jwt)
    token = token_data.get("token")
    expires_at = token_data.get("expires_at")

    os.makedirs(CONFIG_DIR, mode=0o700, exist_ok=True)
    # Mode is applied at open time, so the token is never world-readable, even briefly.
    fd = os.open(TOKEN_FILE_PATH, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        os.fchmod(f.fileno(), 0o600)  # also tightens a pre-existing file
        f.write(token.strip())

    if args.print_token:
        print(token)
    else:
        print(f"[✓] Installation token generated successfully!")
        print(f"    Installation ID: {installation_id}")
        print(f"    Expires at:      {expires_at}")
        print(f"    Token file:      {TOKEN_FILE_PATH} (mode 0600)")

if __name__ == "__main__":
    main()
