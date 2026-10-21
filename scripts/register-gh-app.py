#!/usr/bin/env python3
"""
Register GitHub App for Summa42 via GitHub App Manifest Flow.
"""

import http.server
import hmac
import json
import os
import secrets
import sys
import threading
import urllib.parse
import urllib.request

PORT = 8042
REDIRECT_URI = f"http://localhost:{PORT}/callback"

MANIFEST = {
    "name": f"summa42-bot-{os.environ.get('USER', 'sofiaflux')}",
    "url": "https://github.com/SofiaFlux/summa42",
    "hook_attributes": {
        "url": "https://github.com/SofiaFlux/summa42",
        "active": False
    },
    "redirect_url": REDIRECT_URI,
    "public": False,
    "default_permissions": {
        "issues": "write",
        "pull_requests": "write",
        "contents": "write",
        "statuses": "read",
        "checks": "read"
    },
    "default_events": []
}

HTML_PAGE = f"""<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Register Summa42 GitHub App</title>
  <style>
    body {{
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
      background: #f6f8fa;
      color: #24292f;
      display: flex;
      justify-content: center;
      align-items: center;
      min-height: 100vh;
      margin: 0;
    }}
    .card {{
      background: #ffffff;
      border: 1px solid #d0d7de;
      border-radius: 8px;
      padding: 32px;
      max-width: 520px;
      box-shadow: 0 8px 24px rgba(140,149,159,0.2);
      text-align: center;
    }}
    h1 {{ font-size: 24px; margin-bottom: 12px; }}
    p {{ font-size: 14px; color: #57606a; line-height: 1.5; margin-bottom: 24px; }}
    button {{
      background: #2da44e;
      color: #ffffff;
      border: 1px solid rgba(27,31,36,0.15);
      border-radius: 6px;
      padding: 10px 20px;
      font-size: 16px;
      font-weight: 600;
      cursor: pointer;
      transition: background 0.2s;
    }}
    button:hover {{ background: #2c974b; }}
    .badge {{
      background: #ddf4ff;
      color: #0969da;
      padding: 4px 8px;
      border-radius: 12px;
      font-size: 12px;
      font-weight: 600;
      display: inline-block;
      margin-bottom: 16px;
    }}
  </style>
</head>
<body>
  <div class="card">
    <div class="badge">Summa42 Bot Setup</div>
    <h1>Register GitHub App</h1>
    <p>Click below to register the GitHub App for <strong>SofiaFlux/summa42</strong> with fine-grained permissions (Issues, Pull Requests, Contents).</p>
    <form action="https://github.com/settings/apps/new?state=__STATE__" method="post">
      <input type="hidden" name="manifest" value='{json.dumps(MANIFEST)}'>
      <button type="submit">Create GitHub App from Manifest</button>
    </form>
  </div>
</body>
</html>
"""

captured_code = None
server_done = threading.Event()

class ManifestHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global captured_code
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/":
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            page = HTML_PAGE.replace("__STATE__", urllib.parse.quote(self.server.expected_state))
            self.wfile.write(page.encode("utf-8"))
        elif parsed.path == "/callback":
            params = urllib.parse.parse_qs(parsed.query)
            state = params.get("state", [""])[0]
            if not hmac.compare_digest(state.encode("utf-8"), self.server.expected_state.encode("utf-8")):
                self.send_response(400)
                self.end_headers()
                self.wfile.write(b"State mismatch; callback rejected.")
            elif "code" in params:
                captured_code = params["code"][0]
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.end_headers()
                resp_html = """<!DOCTYPE html>
<html>
<body style="font-family: sans-serif; text-align: center; padding: 40px; background: #f6f8fa;">
  <div style="background: white; border-radius: 8px; padding: 30px; display: inline-block; box-shadow: 0 4px 12px rgba(0,0,0,0.1);">
    <h2 style="color: #2da44e;">&#10004; App Created Successfully!</h2>
    <p>Summa42 is now exchanging credentials with GitHub...</p>
    <p>Please check your terminal to complete installation on the repository.</p>
  </div>
</body>
</html>"""
                self.wfile.write(resp_html.encode("utf-8"))
                server_done.set()
            else:
                self.send_response(400)
                self.end_headers()
                self.wfile.write(b"Missing code in callback query.")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, format, *args):
        pass  # Quiet logging

def make_server(expected_state, port=PORT):
    """Loopback-only callback server that accepts only the expected state."""
    http.server.HTTPServer.allow_reuse_address = True
    server = http.server.HTTPServer(("127.0.0.1", port), ManifestHandler)
    server.expected_state = expected_state
    return server

def write_private(path, text):
    """Create/truncate a file with mode 0600 applied at open time."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        os.fchmod(f.fileno(), 0o600)
        f.write(text)

def convert_code(code):
    req_url = f"https://api.github.com/app-manifests/{code}/conversions"
    req = urllib.request.Request(
        req_url,
        data=b"",
        headers={
            "Accept": "application/vnd.github+json",
            "User-Agent": "summa42-manifest-setup"
        },
        method="POST"
    )
    with urllib.request.urlopen(req) as resp:
        if resp.status != 201:
            raise RuntimeError(f"Unexpected status code {resp.status} from conversion API")
        return json.loads(resp.read().decode("utf-8"))

def main():
    config_dir = os.path.expanduser("~/.summa42")
    os.makedirs(config_dir, mode=0o700, exist_ok=True)

    state = secrets.token_urlsafe(32)
    server = make_server(state)
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()

    print(f"\n=======================================================")
    print(f"  Summa42 GitHub App Registration")
    print(f"=======================================================")
    print(f"1. Open the following URL in your browser:")
    print(f"   http://localhost:{PORT}/")
    print(f"2. Click 'Create GitHub App from Manifest'.")
    print(f"3. GitHub will register the app and redirect back.")
    print(f"=======================================================\n")
    print("Waiting for browser callback (or press Ctrl+C to cancel)...")

    try:
        # Wait up to 15 minutes for the callback
        if not server_done.wait(timeout=900):
            print("\nTimed out waiting for callback.")
            sys.exit(1)
    except KeyboardInterrupt:
        print("\nAborted by user.")
        sys.exit(1)
    finally:
        server.shutdown()

    if not captured_code:
        print("Failed to capture authorization code.")
        sys.exit(1)

    print(f"\n[+] Authorization code captured! Exchanging for credentials...")
    try:
        data = convert_code(captured_code)
    except Exception as e:
        print(f"[-] Error exchanging code: {e}")
        sys.exit(1)

    app_id = data.get("id")
    slug = data.get("slug")
    client_id = data.get("client_id")
    pem_content = data.get("pem")

    pem_path = os.path.join(config_dir, "gh-app.pem")
    json_path = os.path.join(config_dir, "gh-app.json")

    write_private(pem_path, pem_content)

    meta = {
        "app_id": app_id,
        "slug": slug,
        "client_id": client_id,
        "pem_path": pem_path
    }
    write_private(json_path, json.dumps(meta, indent=2))

    print(f"\n[✓] GitHub App created successfully!")
    print(f"    App Name:  {slug}")
    print(f"    App ID:    {app_id}")
    print(f"    Key saved: {pem_path} (mode 0600)")
    print(f"    Config:    {json_path} (mode 0600)")
    print(f"\n-------------------------------------------------------")
    print(f"FINAL STEP: Install the App on your repository:")
    print(f"Open: https://github.com/apps/{slug}/installations/new")
    print(f"and select 'SofiaFlux/summa42'.")
    print(f"-------------------------------------------------------\n")

if __name__ == "__main__":
    main()
