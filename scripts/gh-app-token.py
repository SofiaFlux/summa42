#!/usr/bin/env python3
"""
Generate a short-lived GitHub App Installation Access Token for Summa42.
Saves the token to ~/.summa42/github_token (mode 0600) and prints it.
"""

import argparse
import json
import os
import sys
import time
import urllib.request
import jwt

CONFIG_DIR = os.path.expanduser("~/.summa42")
APP_JSON_PATH = os.path.join(CONFIG_DIR, "gh-app.json")
TOKEN_FILE_PATH = os.path.join(CONFIG_DIR, "github_token")

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

def main():
    parser = argparse.ArgumentParser(description="Generate GitHub App installation token for Summa42")
    parser.add_argument("--output", "-o", default=TOKEN_FILE_PATH, help="Path to write the token file")
    parser.add_argument("--print-token", "-p", action="store_true", help="Print token to stdout")
    args = parser.parse_args()

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

        # Look for SofiaFlux installation
        target_inst = None
        for inst in installations:
            account = inst.get("account", {}).get("login", "")
            if account.lower() == "sofiaflux":
                target_inst = inst
                break
        if not target_inst:
            target_inst = installations[0]

        installation_id = target_inst["id"]
        meta["installation_id"] = installation_id
        meta["installation_account"] = target_inst.get("account", {}).get("login")
        with open(APP_JSON_PATH, "w") as f:
            json.dump(meta, f, indent=2)

    token_data = get_installation_token(installation_id, app_jwt)
    token = token_data.get("token")
    expires_at = token_data.get("expires_at")

    out_path = os.path.expanduser(args.output)
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "w") as f:
        f.write(token.strip())
    os.chmod(out_path, 0o600)

    if args.print_token:
        print(token)
    else:
        print(f"[✓] Installation token generated successfully!")
        print(f"    Installation ID: {installation_id}")
        print(f"    Expires at:      {expires_at}")
        print(f"    Token file:      {out_path} (mode 0600)")

if __name__ == "__main__":
    main()
