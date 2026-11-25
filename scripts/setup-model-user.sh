#!/usr/bin/env bash
# One-time setup of the dedicated OS user that Summa42 model processes run as.
#
# Run as:  sudo scripts/setup-model-user.sh [--yes|--default-approve]
#            [--summa-home DIR] [--db FILE] [--evidence DIR] [--toolchain-dirs DIRS]
#
# Idempotent. It prints every step. It creates three system users, each with its
# own home, no login shell and no extra groups:
#   summa42-model     runs the producer CLIs (may hold their copied logins)
#   summa42-review    runs the reviewer CLIs and Copilot (isolated home)
#   summa42-validate  runs validation commands, i.e. candidate code; it has NO
#                     credentials, never receives a copy of any, and a scan
#                     refuses if it finds any in its home
# It authorises the invoking user to run commands as each of them (and only as
# them) via sudoers, locks the invoking user's ~/.summa42 and ~/.config/gh and
# the model/review homes to mode 0700, and then PROVES neither can read the Box
# secrets, model and review cannot read each other's homes, and validate cannot
# read either model or review home. It exits non-zero if any secret is readable
# or if a configured secret path cannot be evaluated.
#
# Trust boundary: each dedicated user owns its home and a prompt-injected model
# can plant symlinks there. Root therefore never reads, writes, chmods or chowns
# anything inside those homes after `useradd`: every file operation there is
# done BY that user (run_as_model / run_as_review). Root only creates the users
# and writes /etc/sudoers.d.
set -euo pipefail

MODEL_USER="${SUMMA42_MODEL_USER:-summa42-model}"
REVIEW_USER="${SUMMA42_REVIEW_MODEL_USER:-summa42-review}"
VALIDATE_USER="${SUMMA42_VALIDATION_USER:-summa42-validate}"
SUDOERS_FILE="/etc/sudoers.d/summa42-model"
SUDOERS_REVIEW_FILE="/etc/sudoers.d/summa42-review"
SUDOERS_VALIDATE_FILE="/etc/sudoers.d/summa42-validate"
# Colon-separated absolute directories holding the Go toolchain the validation
# user runs (sudo strips the environment, so --toolchain-dirs also sets it).
TOOLCHAIN_DIRS="${SUMMA42_VALIDATION_TOOLCHAIN_DIRS:-}"
WSL_INTEROP_FILE="${WSL_INTEROP_FILE:-/proc/sys/fs/binfmt_misc/WSLInterop}"
PROC_VERSION_FILE="${PROC_VERSION_FILE:-/proc/version}"

say() { printf '==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- pure helpers

# A system account name: lowercase, digits, _ and -, at most 32 chars. This is
# also what is written into sudoers, so nothing else may reach it.
valid_model_user() { [[ "$1" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; }
# The invoking account may also carry dots and capitals.
valid_owner_name() { [[ "$1" =~ ^[A-Za-z_][A-Za-z0-9._-]{0,63}$ ]]; }

# The whole drop-in. SETENV lets the Box pass allowlisted variables with
# sudo --preserve-env (values travel in the environment, not on argv), and
# !log_allowed keeps command lines (model prompts, paths) out of the sudo log.
sudoers_content() {
  printf '%s ALL=(%s) NOPASSWD: SETENV: ALL\n' "$1" "$2"
  printf 'Defaults>%s !log_allowed\n' "$2"
}

is_wsl() {
  [ -e "$WSL_INTEROP_FILE" ] && return 0
  grep -qi microsoft "$PROC_VERSION_FILE" 2>/dev/null
}

wsl_warning() {
  cat >&2 <<'WSL'
WARNING: this is WSL. The isolation is INCOMPLETE here while Windows interop is
on and /mnt/c is world-readable: the model user can read Windows files (for
example the Windows GitHub CLI token under /mnt/c/Users/*/AppData) and can
start Windows executables as the Windows user, which can reach the Box's files
through \\wsl.localhost. Linux uid separation does not cover that.
To close it, put this in /etc/wsl.conf and run `wsl --shutdown` from Windows:
    [interop]
    enabled=false
    appendWindowsPath=false
    [automount]
    options="metadata,umask=077,fmask=077"
If you need Windows interop in your own shell (browser logins, explorer.exe),
leave /etc/wsl.conf alone and run the Box in a systemd unit that cannot see
Windows; the model users it starts inherit that:
    sudo systemd-run --unit=summa42-loop --uid=$USER -p InaccessiblePaths=/mnt/c \
      -p InaccessiblePaths=/run/WSL <the Box command>
This script does not change /etc/wsl.conf and does not refuse to continue.
WSL
}

# Content patterns that must never be copied to the model user. Copilot's own
# OAuth tokens (gho_/ghu_) are the credential being copied there, so that mode
# only refuses the broader GitHub token kinds. Azure DevOps PATs come as 52
# chars of base32 (old) or 84 chars of base64url-ish (new), quoted or not, so a
# value of either length after `:` or `=` is refused, as is any value in a
# *PAT / AZURE_DEVOPS_* / ADO_* key and any Authorization Bearer/Basic header.
secret_pattern() {
  local base='ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|ghs_[A-Za-z0-9]{20,}|ghr_[A-Za-z0-9]{20,}'
  base+='|"[a-z2-7]{52}"|"[A-Za-z0-9_-]{84}"'
  base+='|[:=] *"?[a-z2-7]{52}"?([^A-Za-z0-9]|$)|[:=] *"?[A-Za-z0-9_-]{84}"?([^A-Za-z0-9_-]|$)'
  base+='|(AZURE_DEVOPS_[A-Z_]*|ADO_[A-Z_]*|[A-Z_]*_PAT|SYSTEM_ACCESSTOKEN)"? *[:=]'
  base+='|[Bb]earer +[A-Za-z0-9._~+/=-]{16,}|[Bb]asic +[A-Za-z0-9+/=]{16,}'
  base+='|-----BEGIN [A-Z ]*PRIVATE KEY-----'
  if [ "${1:-}" = copilot ]; then printf '%s' "$base"; else printf '%s|gh[ou]_[A-Za-z0-9]{20,}' "$base"; fi
}

# scan_secrets MODE FILE...  prints the files holding a token pattern; returns 0
# when at least one does. -a: a file grep would call binary is scanned as text
# rather than skipped.
scan_secrets() {
  local mode="$1"; shift
  local hits
  hits="$(grep -laE -e "$(secret_pattern "$mode")" -- "$@" 2>/dev/null || true)"
  [ -n "$hits" ] || return 1
  printf '%s\n' "$hits"
}

# Keep only what Claude Code needs to treat the credentials as a finished login.
# Everything else in ~/.claude.json (mcpServers env tokens, project history) is dropped.
filter_claude_json() {
  python3 - "$1" "$2" <<'PY'
import json, sys
src = json.load(open(sys.argv[1]))
keep = {k: src[k] for k in ("hasCompletedOnboarding", "oauthAccount", "userID") if k in src}
json.dump(keep, open(sys.argv[2], "w"))
PY
}

# Gemini's settings.json carries mcpServers (env, headers), hooks and tools next
# to the login type. Keep only security.auth (selectedType, enforcedType,
# useExternal), scalars only.
filter_gemini_settings() {
  python3 - "$1" "$2" <<'PY'
import json, sys
src = json.load(open(sys.argv[1]))
auth = ((src.get("security") or {}).get("auth")) or {}
keep = {k: v for k, v in auth.items() if isinstance(v, (str, bool, int)) and not isinstance(v, dict)}
json.dump({"security": {"auth": keep}}, open(sys.argv[2], "w"))
PY
}

# chmod_owner_dir DIR: only a real directory, never through a symlink.
chmod_owner_dir() {
  if [ -d "$1" ] && [ ! -L "$1" ]; then
    say "chmod 0700 $1"
    chmod 0700 -- "$1"
  elif [ -L "$1" ]; then
    warn "$1 is a symlink; not touching it"
  else
    say "$1 does not exist; skipping"
  fi
}

# Run a command as a dedicated user, with a clean environment and cwd /. Root
# never touches such a home itself; this is the only way it is reached.
run_as() {
  local who="$1" home="$2"; shift 2
  (
    cd /
    if command -v runuser >/dev/null 2>&1; then
      exec runuser -u "$who" -- env -i HOME="$home" PATH=/usr/local/bin:/usr/bin:/bin "$@"
    else
      exec sudo -n -u "$who" -- env -i HOME="$home" PATH=/usr/local/bin:/usr/bin:/bin "$@"
    fi
  )
}
run_as_model() { run_as "$MODEL_USER" "$MODEL_HOME" "$@"; }
run_as_review() { run_as "$REVIEW_USER" "$REVIEW_HOME" "$@"; }

role_home() {
  case "${1:-model}" in
    review) printf '%s' "$REVIEW_HOME" ;;
    validate) printf '%s' "$VALIDATE_HOME" ;;
    *) printf '%s' "$MODEL_HOME" ;;
  esac
}

# audit_scope_tokens SCOPE...: return 0 if any token declares a write/admin/CI scope.
has_write_scopes() {
  local tok
  for tok in "$@"; do
    tok="$(printf '%s' "$tok" | tr '[:upper:]' '[:lower:]' | tr -d '",')"
    case "$tok" in
      repo|public_repo|workflow|delete_repo|gist|write:*|admin:*|vso.code_write|vso.code_manage|vso.build_execute|*_write)
        return 0 ;;
    esac
  done
  return 1
}

# audit_login_scopes FILE...: inspect JSON payloads recursively for scope/scopes
# declaring repo-write or CI-write privileges. Exits 2 and names offending files.
audit_login_scopes() {
  command -v python3 >/dev/null 2>&1 || return 0
  python3 - "$@" <<'PY'
import json, re, sys

FORBIDDEN_EXACT = {"repo", "public_repo", "workflow", "delete_repo", "gist", "vso.code_write", "vso.code_manage", "vso.build_execute"}
def is_forbidden(tok: str) -> bool:
    s = tok.strip().lower()
    return bool(s) and (s in FORBIDDEN_EXACT or s.startswith("write:") or s.startswith("admin:") or s.endswith("_write"))

def walk(obj):
    if isinstance(obj, dict):
        for k, v in obj.items():
            if str(k).lower() in ("scope", "scopes"):
                if isinstance(v, str):
                    for part in re.split(r"[\s,]+", v):
                        if is_forbidden(part):
                            yield part
                elif isinstance(v, list):
                    for item in v:
                        if isinstance(item, str) and is_forbidden(item):
                            yield item
            yield from walk(v)
    elif isinstance(obj, list):
        for item in obj:
            yield from walk(item)

bad = []
for path in sys.argv[1:]:
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except Exception:
        continue
    found = sorted(set(walk(data)))
    if found:
        bad.append(f"{path}: {','.join(found)}")
if bad:
    for line in bad:
        print(line, file=sys.stderr)
    sys.exit(2)
PY
}

# check_github_oauth_scopes TOKEN: ask GitHub API via curl config on stdin so the
# token never lands on argv, and print X-OAuth-Scopes on HTTP 200.
check_github_oauth_scopes() {
  local tok="$1" headers status scopes
  command -v curl >/dev/null 2>&1 || return 1
  headers="$(curl -sS --max-time 10 -I -K - 2>/dev/null <<CFG | tr -d '\r' || true
url = "https://api.github.com/"
header = "User-Agent: summa42-setup"
header = "Authorization: token $tok"
CFG
)"
  status="$(printf '%s\n' "$headers" | awk 'toupper($1) ~ /^HTTP\// {code=$2} END{print code}')"
  [ "$status" = "200" ] || return 1
  scopes="$(printf '%s\n' "$headers" | awk 'BEGIN{IGNORECASE=1} /^x-oauth-scopes:[[:space:]]*/{sub(/^[^:]*:[[:space:]]*/, ""); print; exit}')"
  printf '%s\n' "$scopes"
}
run_as_validate() { run_as "$VALIDATE_USER" "$VALIDATE_HOME" "$@"; }

# The validation user must hold no credentials at all: refuse if its home has
# any known CLI login file or anything matching a token pattern. Everything is
# read BY the validation user, never by root. Its Go caches and scratch are
# not scanned (they hold compiled output, not logins).
validate_home_clean() {
  local rel cli found="" hits
  for cli in claude codex commandcode gemini copilot; do
    for rel in $(group_files "$cli") .claude.json .config/gh/hosts.yml .netrc .git-credentials .ssh; do
      if run_as_validate test -e "$VALIDATE_HOME/$rel"; then found+="$VALIDATE_HOME/$rel"$'\n'; fi
    done
  done
  hits="$(run_as_validate grep -rlaE --exclude-dir=go-cache --exclude-dir=tmp -e "$(secret_pattern default)" -- "$VALIDATE_HOME" 2>/dev/null || true)"
  [ -z "$hits" ] || found+="$hits"$'\n'
  [ -z "$found" ] && return 0
  printf 'the validation user %s must hold no credentials, but its home has:\n%s' "$VALIDATE_USER" "$found" >&2
  return 1
}

# Runs as the model user: unpack one relative path from stdin and rename it into place.
MODEL_INSTALL_SH='
umask 077
rel=$1
case "$rel" in /*|*..*) exit 1 ;; esac
dest="$HOME/$rel"
mkdir -p -- "$(dirname -- "$dest")" || exit 1
stage=$(mktemp -d "$HOME/.stage.XXXXXX") || exit 1
trap "rm -rf \"$stage\"" EXIT
tar -xf - -C "$stage" || exit 1
chmod 0600 -- "$stage/$rel" || exit 1
mv -T -- "$stage/$rel" "$dest"
'

# copy_file SRCROOT REL [ROLE]: stream SRCROOT/REL into the role home as that user.
copy_file() {
  local role="${3:-model}"
  tar -C "$1" -cf - -- "$2" | "run_as_$role" sh -c "$MODEL_INSTALL_SH" sh "$2"
}

# Safe source files: regular, not symlinks, inside the owner's home, and not a
# Box secret location.
safe_source() {
  local f="$1" real
  [ -f "$f" ] && [ ! -L "$f" ] || return 1
  real="$(readlink -f -- "$f")"
  case "$real" in
    "$SUMMA_HOME"|"$SUMMA_HOME"/*|"$OWNER_HOME/.config/gh"|"$OWNER_HOME/.config/gh"/*) return 1 ;;
    "$OWNER_HOME"/*) return 0 ;;
  esac
  return 1
}

# The minimal auth files of each CLI. Session transcripts, history, projects,
# plugins and MCP configuration are deliberately NOT offered.
#   claude:  ~/.claude/.credentials.json (OAuth tokens) + a filtered ~/.claude.json
#            (login metadata only; mcpServers and project history are dropped)
#   codex:   ~/.codex/auth.json
#   gemini / agy (Antigravity shares the Gemini login): oauth_creds.json,
#            google_accounts.json, installation_id, settings.json (only its
#            security.auth section is copied)
#   copilot: ~/.copilot/config.json, ~/.copilot/mcp-config.json (the executor's
#            MCP server definition, which Copilot reads from its own home),
#            ~/.config/github-copilot/{apps,hosts}.json
group_files() {
  case "$1" in
    claude)  echo ".claude/.credentials.json" ;;
    codex)   echo ".codex/auth.json" ;;
    commandcode) echo ".commandcode/auth.json" ;;
    gemini)  echo ".gemini/oauth_creds.json .gemini/google_accounts.json .gemini/installation_id .gemini/settings.json" ;;
    copilot) echo ".copilot/config.json .copilot/mcp-config.json .config/github-copilot/apps.json .config/github-copilot/hosts.json" ;;
  esac
}

# Every login file a CLI may have in the model home, each listed once.
model_home_login_files() {
  local cli
  for cli in claude codex commandcode gemini copilot; do group_files "$cli"; done | tr ' ' '\n'
  echo .claude.json
}

approve() {
  # approve LABEL DETAIL: --yes approves, otherwise ask on a TTY.
  [ "$ASSUME_YES" -eq 1 ] && return 0
  ask "$@"
}
ask() {
  [ -t 0 ] || return 1
  local answer
  printf '  %s -> %s ? [y/N] ' "$1" "$2"
  read -r answer || return 1
  case "$answer" in y|Y|yes|YES|Yes) return 0 ;; *) return 1 ;; esac
}

# import_cli NAME [ROLE]: copy one CLI's auth files after secret + scope audits.
# ROLE defaults to model; review installs into REVIEW_HOME as REVIEW_USER.
# Returns 2 when a token pattern was found, 3 when any step failed.
import_cli() {
  local rc=0
  stagedir="$(mktemp -d)" || return 3
  if [ -z "$stagedir" ] || [ "$stagedir" = / ] || [ ! -d "$stagedir" ]; then
    printf 'cannot create a staging directory\n' >&2; return 3
  fi
  _import_cli "$1" "${2:-model}" || rc=$?
  rm -rf -- "$stagedir"
  return "$rc"
}

_import_cli() {
  local name="$1" role="${2:-model}" rel files=() mode=default dest_home
  dest_home="$(role_home "$role")"
  for rel in $(group_files "$name"); do
    if safe_source "$OWNER_HOME/$rel"; then files+=("$rel"); fi
  done
  # ~/.claude.json only rides along with a real login.
  if [ "$name" = claude ] && [ "${#files[@]}" -gt 0 ] && [ -f "$OWNER_HOME/.claude.json" ] && [ ! -L "$OWNER_HOME/.claude.json" ]; then
    files+=(.claude.json)
  fi
  if [ "${#files[@]}" -eq 0 ]; then return 0; fi
  [ "$name" = copilot ] && mode=copilot
  local scan=() staged=() filtered kept=()
  for rel in "${files[@]}"; do
    filtered=""
    case "$rel" in
      .claude.json) filtered=filter_claude_json ;;
      .gemini/settings.json) filtered=filter_gemini_settings ;;
    esac
    if [ -n "$filtered" ]; then
      # Never fall back to copying the unfiltered file.
      if ! command -v python3 >/dev/null 2>&1; then warn "python3 missing; not copying $rel"; continue; fi
      mkdir -p -- "$stagedir/$(dirname -- "$rel")" || return 3
      "$filtered" "$OWNER_HOME/$rel" "$stagedir/$rel" || { printf 'cannot filter %s\n' "$rel" >&2; return 3; }
      scan+=("$stagedir/$rel"); staged+=("$rel")
    else
      scan+=("$OWNER_HOME/$rel")
    fi
    kept+=("$rel")
  done
  files=("${kept[@]}")
  if [ "${#files[@]}" -eq 0 ]; then return 0; fi
  local hits
  if hits="$(scan_secrets "$mode" "${scan[@]}")"; then
    printf 'refusing to copy %s: token patterns found in:\n%s\n' "$name" "$hits" >&2
    return 2
  fi
  if [ "$name" = copilot ]; then
    local audited_files=() audited_scan=() i toks tok verified scopes
    for ((i = 0; i < ${#files[@]}; i++)); do
      rel="${files[$i]}"
      if [ "$rel" != ".copilot/mcp-config.json" ]; then
        toks="$(grep -oE 'gho_[A-Za-z0-9]{20,}' -- "${scan[$i]}" 2>/dev/null || true)"
        if [ -n "$toks" ]; then
          verified=1
          while IFS= read -r tok; do
            [ -n "$tok" ] || continue
            if ! scopes="$(check_github_oauth_scopes "$tok")"; then
              verified=0; break
            fi
            # Split commas/spaces carefully before checking scopes.
            local scope_words=()
            IFS=', ' read -ra scope_words <<<"$scopes"
            if [ "${#scope_words[@]}" -gt 0 ] && has_write_scopes "${scope_words[@]}"; then
              verified=0; break
            fi
          done <<<"$toks"
          if [ "$verified" -eq 0 ]; then
            warn "dropping $rel for $role ($dest_home): GitHub OAuth token is over-scoped or could not be verified read-only"
            continue
          fi
        fi
      fi
      audited_files+=("$rel"); audited_scan+=("${scan[$i]}")
    done
    files=("${audited_files[@]}")
    scan=("${audited_scan[@]}")
    if [ "${#files[@]}" -eq 0 ]; then skipped+=("$role:$name"); return 0; fi
  fi
  if hits="$(audit_login_scopes "${scan[@]}" 2>&1)"; then
    :
  else
    warn "dropping $name for $role ($dest_home): login declares write scopes ($hits)"
    skipped+=("$role:$name")
    return 0
  fi
  local verb=copy dest_exists=0
  for rel in "${files[@]}"; do
    if "run_as_$role" test -e "$dest_home/$rel"; then dest_exists=1; fi
  done
  [ "$dest_exists" -eq 1 ] && verb=refresh
  if [ "$name" = copilot ]; then
    warn "copilot's login is a GitHub OAuth credential; copying it hands the model a GitHub credential."
    if [ "$ASSUME_YES" -eq 1 ]; then
      say "  skipped copilot: never copied under --yes; re-run without --yes to be asked"
      skipped+=("$role:$name"); return 0
    fi
    ask "$verb $name auth (${files[*]})" "$dest_home" || { say "  skipped $name"; skipped+=("$role:$name"); return 0; }
  else
    approve "$verb $name auth (${files[*]})" "$dest_home" || { say "  skipped $name (any existing copy is left untouched)"; skipped+=("$role:$name"); return 0; }
  fi
  local n=0
  for rel in "${files[@]}"; do
    if [[ " ${staged[*]:-} " == *" $rel "* ]]; then
      copy_file "$stagedir" "$rel" "$role" || { printf 'copying %s failed\n' "$rel" >&2; return 3; }
    else
      copy_file "$OWNER_HOME" "$rel" "$role" || { printf 'copying %s failed\n' "$rel" >&2; return 3; }
    fi
    n=$((n + 1))
  done
  say "  done: $verb $name for $role ($n file(s))"
  copied+=("$role:$name")
}

# purge_overscoped_logins ROLE HOME: remove previously copied login files whose
# JSON scopes or Copilot gho_ tokens are over-scoped (or unverified).
purge_overscoped_logins() {
  local role="$1" home="$2" rel tmp
  tmp="$(mktemp)" || return 1
  for rel in $(model_home_login_files); do
    [ "$rel" = ".copilot/mcp-config.json" ] && continue
    if "run_as_$role" test -f "$home/$rel"; then
      if "run_as_$role" cat "$home/$rel" >"$tmp" 2>/dev/null; then
        local bad=0
        if ! audit_login_scopes "$tmp" >/dev/null 2>&1; then
          bad=1
        elif [[ "$rel" == *copilot* || "$rel" == *github-copilot* ]]; then
          local toks tok scopes
          toks="$(grep -oE 'gho_[A-Za-z0-9]{20,}' -- "$tmp" 2>/dev/null || true)"
          if [ -n "$toks" ]; then
            while IFS= read -r tok; do
              [ -n "$tok" ] || continue
              if ! scopes="$(check_github_oauth_scopes "$tok")"; then
                bad=1; break
              fi
              local scope_words=()
              IFS=', ' read -ra scope_words <<<"$scopes"
              if [ "${#scope_words[@]}" -gt 0 ] && has_write_scopes "${scope_words[@]}"; then
                bad=1; break
              fi
            done <<<"$toks"
          fi
        fi
        if [ "$bad" -eq 1 ]; then
          warn "removing stale over-scoped login $home/$rel"
          "run_as_$role" rm -f -- "$home/$rel"
        fi
      fi
    fi
  done
  rm -f -- "$tmp"
}

verify_cross_home_isolation() {
  local rel
  say "verifying $VALIDATE_USER cannot read $MODEL_HOME or $REVIEW_HOME"
  require_unreadable "$MODEL_HOME" required validate
  require_unreadable "$REVIEW_HOME" required validate
  say "verifying $MODEL_USER and $REVIEW_USER cannot read each other's homes"
  require_unreadable "$MODEL_HOME" required review
  require_unreadable "$REVIEW_HOME" required model
  for rel in $(model_home_login_files); do
    require_unreadable "$MODEL_HOME/$rel" optional validate
    require_unreadable "$REVIEW_HOME/$rel" optional validate
    require_unreadable "$MODEL_HOME/$rel" optional review
    require_unreadable "$REVIEW_HOME/$rel" optional model
  done
}

# Resolve one configured path as the invoking user. Prints the path or nothing.
owner_config_value() {
  command -v python3 >/dev/null 2>&1 || return 0
  [ -f "$SUMMA_HOME/config.json" ] || return 0
  sudo -n -u "$OWNER" -- python3 -c '
import json, sys
print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$SUMMA_HOME/config.json" "$1" 2>/dev/null || true
}

fail=0
declare -A PROBED_PATHS=()
# require_unreadable PATH KIND [WHO]: KIND is "required" (must exist) or
# "optional"; WHO is model (default) or validate.
require_unreadable() {
  local path="$1" kind="$2" who="${3:-model}"
  if [ -n "${PROBED_PATHS["$who:$path"]:-}" ]; then
    return 0
  fi
  PROBED_PATHS["$who:$path"]=1
  if [ ! -e "$path" ]; then
    if [ "$kind" = required ]; then
      printf '  MISSING (cannot evaluate): %s\n' "$path" >&2; fail=1
    fi
    return 0
  fi
  if "run_as_$who" test -r "$path"; then
    printf '  READABLE by %s (BAD): %s\n' "$who" "$path" >&2; fail=1
  else
    printf '  ok, not readable by %s: %s\n' "$who" "$path"
  fi
}

# ensure_user NAME PURPOSE: create a dedicated system user (or accept an existing
# nologin system account) and set ENSURED_HOME. Root only creates the user.
ensure_user() {
  local name="$1" purpose="$2" shell uid uid_min
  if id "$name" >/dev/null 2>&1; then
    shell="$(getent passwd "$name" | cut -d: -f7)"
    uid="$(id -u "$name")"
    uid_min="$(awk '$1=="UID_MIN"{print $2}' /etc/login.defs 2>/dev/null || true)"
    case "$shell" in */nologin|*/false) ;; *) die "existing user $name has login shell $shell; refusing to use a real account" ;; esac
    [ "$uid" -lt "${uid_min:-1000}" ] || die "existing user $name (uid $uid) is not a system account; refusing to use it"
    say "user $name already exists; leaving it as is"
  else
    say "creating system user $name ($purpose; home /var/lib/$name, no login shell, no groups)"
    useradd --system --create-home --home-dir "/var/lib/$name" \
      --shell /usr/sbin/nologin --user-group "$name"
  fi
  ENSURED_HOME="$(getent passwd "$name" | cut -d: -f6)"
  [ "$(id -u "$name")" -ne 0 ] || die "$name resolved to uid 0"
  local extra_groups
  extra_groups="$(id -nG "$name" | tr ' ' '\n' | grep -vx "$name" || true)"
  [ -z "$extra_groups" ] || die "$name belongs to extra groups ($extra_groups); remove them"
}

# install_sudoers FILE USER: validated as a fragment, installed, then the whole
# configuration is re-checked and the drop-in rolled back if that fails.
install_sudoers() {
  local file="$1" target="$2" tmp backup had_old=0
  tmp="$(mktemp)"; backup="$(mktemp)"
  sudoers_content "$OWNER" "$target" >"$tmp"
  if ! visudo -cf "$tmp" >/dev/null; then rm -f "$tmp" "$backup"; die "generated sudoers entry for $target failed visudo -cf"; fi
  local pre
  if ! pre="$(visudo -c 2>&1)"; then
    rm -f "$tmp" "$backup"
    die "the existing sudoers configuration already fails visudo -c; fix it first (nothing was written):
$pre"
  fi
  if [ -f "$file" ] && cmp -s "$tmp" "$file"; then
    say "$file already up to date; leaving it as is"
  else
    if [ -f "$file" ]; then cp -p -- "$file" "$backup"; had_old=1; fi
    say "writing $file: $OWNER may run commands as $target without a password (sudo does not log them)"
    install -m 0440 -o root -g root "$tmp" "$file.new"
    mv -f "$file.new" "$file"
    if ! visudo -c >/dev/null; then
      if [ "$had_old" -eq 1 ]; then mv -f "$backup" "$file"; else rm -f "$file"; fi
      rm -f "$tmp" "$backup"
      die "full sudoers configuration failed visudo -c with the new drop-in for $target; rolled it back"
    fi
  fi
  rm -f "$tmp" "$backup"
}

# check_toolchain: the Go toolchain named in TOOLCHAIN_DIRS must run as the
# validation user (it validates this repo with GOTOOLCHAIN=local, so it must be
# the version the repo needs, e.g. go1.27.x under /usr/local/go1.27).
check_toolchain() {
  [ -n "$TOOLCHAIN_DIRS" ] || return 0
  local dir out dirs
  IFS=: read -ra dirs <<<"$TOOLCHAIN_DIRS"
  for dir in "${dirs[@]}"; do
    case "$dir" in /*) ;; *) die "toolchain dir $dir must be absolute" ;; esac
  done
  say "checking that the validation user can run the Go toolchain in $TOOLCHAIN_DIRS"
  if ! out="$(run_as_validate env GOTOOLCHAIN=local PATH="$TOOLCHAIN_DIRS:/usr/local/bin:/usr/bin:/bin" go version 2>&1)"; then
    die "\`go version\` fails as $VALIDATE_USER with PATH=$TOOLCHAIN_DIRS: $out (the directories must be readable and executable by $VALIDATE_USER, e.g. install Go under /usr/local/go1.27, not under $OWNER_HOME)"
  fi
  say "  $out"
}

main() {
  ASSUME_YES=0
  PROBED_PATHS=()
  local arg_home="" arg_db="" arg_evidence=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --yes|--default-approve) ASSUME_YES=1 ;;
      --summa-home) arg_home="${2:?}"; shift ;;
      --db) arg_db="${2:?}"; shift ;;
      --evidence) arg_evidence="${2:?}"; shift ;;
      --toolchain-dirs) TOOLCHAIN_DIRS="${2:?}"; shift ;;
      *) printf 'usage: %s [--yes|--default-approve] [--summa-home DIR] [--db FILE] [--evidence DIR] [--toolchain-dirs DIRS]\n' "$0" >&2; exit 2 ;;
    esac
    shift
  done
  valid_model_user "$MODEL_USER" || die "SUMMA42_MODEL_USER=$MODEL_USER is not a valid system user name (^[a-z_][a-z0-9_-]{0,31}\$)"
  valid_model_user "$REVIEW_USER" || die "SUMMA42_REVIEW_MODEL_USER=$REVIEW_USER is not a valid system user name (^[a-z_][a-z0-9_-]{0,31}\$)"
  valid_model_user "$VALIDATE_USER" || die "SUMMA42_VALIDATION_USER=$VALIDATE_USER is not a valid system user name (^[a-z_][a-z0-9_-]{0,31}\$)"
  [ "$REVIEW_USER" != "$MODEL_USER" ] || die "the review model user must differ from the model user: reviewer and producer must be isolated"
  [ "$VALIDATE_USER" != "$MODEL_USER" ] || die "the validation user must differ from the model user: candidate code must not run with the model's logins"
  [ "$VALIDATE_USER" != "$REVIEW_USER" ] || die "the validation user must differ from the review model user: candidate code must not run with the reviewer's logins"

  [ "$(id -u)" -eq 0 ] || die "run with sudo: sudo $0"
  OWNER="${SUDO_USER:-}"
  [ -n "$OWNER" ] && [ "$OWNER" != root ] || die "run via sudo from the Box owner's account (SUDO_USER is unset or root)"
  valid_owner_name "$OWNER" || die "unusual owner account name: $OWNER"
  [ "$OWNER" != "$MODEL_USER" ] || die "the model user must differ from the Box owner"
  [ "$OWNER" != "$REVIEW_USER" ] || die "the review model user must differ from the Box owner"
  [ "$OWNER" != "$VALIDATE_USER" ] || die "the validation user must differ from the Box owner"
  OWNER_HOME="$(getent passwd "$OWNER" | cut -d: -f6)"
  [ -d "$OWNER_HOME" ] || die "cannot find the home directory of $OWNER"
  SUMMA_HOME="${arg_home:-${SUMMA42_HOME:-$OWNER_HOME/.summa42}}"
  [ -d "$SUMMA_HOME" ] || die "Summa42 home $SUMMA_HOME does not exist; pass --summa-home DIR (sudo strips SUMMA42_HOME from the environment)"

  if is_wsl; then wsl_warning; fi

  # 1. The users. Root creates them and their homes, nothing more.
  ensure_user "$MODEL_USER" "runs the producer CLIs"
  MODEL_HOME="$ENSURED_HOME"
  ensure_user "$REVIEW_USER" "runs the reviewer CLIs and Copilot"
  REVIEW_HOME="$ENSURED_HOME"
  ensure_user "$VALIDATE_USER" "runs validation commands, i.e. candidate code; holds no credentials"
  VALIDATE_HOME="$ENSURED_HOME"
  [ "$MODEL_HOME" != "$REVIEW_HOME" ] || die "the model and review users share the home $MODEL_HOME"
  [ "$MODEL_HOME" != "$VALIDATE_HOME" ] || die "the model and validation users share the home $MODEL_HOME"
  [ "$REVIEW_HOME" != "$VALIDATE_HOME" ] || die "the review and validation users share the home $REVIEW_HOME"
  [ "$(id -u "$MODEL_USER")" -ne "$(id -u "$REVIEW_USER")" ] || die "the model and review users share uid $(id -u "$MODEL_USER")"
  [ "$(id -u "$MODEL_USER")" -ne "$(id -u "$VALIDATE_USER")" ] || die "the model and validation users share uid $(id -u "$MODEL_USER")"
  [ "$(id -u "$REVIEW_USER")" -ne "$(id -u "$VALIDATE_USER")" ] || die "the review and validation users share uid $(id -u "$REVIEW_USER")"
  say "ensuring $MODEL_HOME and $MODEL_HOME/tmp are private to $MODEL_USER (0700), as $MODEL_USER"
  run_as_model chmod 0700 "$MODEL_HOME"
  run_as_model mkdir -p -m 0700 "$MODEL_HOME/tmp"
  say "ensuring $REVIEW_HOME and $REVIEW_HOME/tmp are private to $REVIEW_USER (0700), as $REVIEW_USER"
  run_as_review chmod 0700 "$REVIEW_HOME"
  run_as_review mkdir -p -m 0700 "$REVIEW_HOME/tmp"
  say "ensuring $VALIDATE_HOME and $VALIDATE_HOME/tmp are private to $VALIDATE_USER (0700), as $VALIDATE_USER"
  run_as_validate chmod 0700 "$VALIDATE_HOME"
  run_as_validate mkdir -p -m 0700 "$VALIDATE_HOME/tmp"
  validate_home_clean || die "refusing to continue; remove the credentials from $VALIDATE_HOME (nothing is ever copied there)"

  # 2. sudoers drop-ins, one per user.
  install_sudoers "$SUDOERS_FILE" "$MODEL_USER"
  install_sudoers "$SUDOERS_REVIEW_FILE" "$REVIEW_USER"
  install_sudoers "$SUDOERS_VALIDATE_FILE" "$VALIDATE_USER"

  # 3. Lock the Box owner's secret directories (never through a symlink).
  chmod_owner_dir "$SUMMA_HOME"
  chmod_owner_dir "$OWNER_HOME/.config/gh"

  # 3b. Purge stale over-scoped logins, then copy minimal role-separated auth.
  purge_overscoped_logins model "$MODEL_HOME"
  purge_overscoped_logins review "$REVIEW_HOME"
  copied=(); skipped=()
  say "CLI auth import (asks per CLI and role; --yes approves non-Copilot CLIs; nothing is copied without a TTY or --yes)"
  local cli role rc
  for role in model review; do
    for cli in claude codex commandcode gemini copilot; do
      [ "$cli" = copilot ] && [ "$role" = model ] && continue
      rc=0; import_cli "$cli" "$role" || rc=$?
      case "$rc" in
        0) ;;
        2) die "refusing to continue; a token pattern was found in $cli's files and nothing from $cli was copied" ;;
        *) die "copying the $cli auth files for $role failed (exit $rc); the copy may be incomplete; fix the error above and re-run" ;;
      esac
    done
  done
  if [ "${#skipped[@]}" -gt 0 ]; then
    say "skipped (log in directly as $MODEL_USER / $REVIEW_USER instead, or re-run): ${skipped[*]}"
  fi

  # 4. Prove dedicated users cannot read the secrets or cross-role homes.
  say "verifying $MODEL_USER, $REVIEW_USER and $VALIDATE_USER cannot read the Box secrets"
  run_as_model test -r /etc/passwd || die "cannot run commands as $MODEL_USER; the readability check would be vacuous"
  run_as_review test -r /etc/passwd || die "cannot run commands as $REVIEW_USER; the readability check would be vacuous"
  run_as_validate test -r /etc/passwd || die "cannot run commands as $VALIDATE_USER; the readability check would be vacuous"
  local db evidence
  db="${arg_db:-$(owner_config_value database_path)}"
  evidence="${arg_evidence:-$(owner_config_value evidence_path)}"
  [ -n "$db" ] || die "cannot determine the state DB path (no database_path in $SUMMA_HOME/config.json); pass --db FILE"
  [ -n "$evidence" ] || die "cannot determine the evidence path; pass --evidence DIR"
  local who
  for who in model review validate; do
    require_unreadable "$SUMMA_HOME" required "$who"
    require_unreadable "$SUMMA_HOME/keys" required "$who"
    require_unreadable "$SUMMA_HOME/keys/owner.key" optional "$who"
    local k
    for k in "$SUMMA_HOME"/keys/*; do
      [ -e "$k" ] && require_unreadable "$k" required "$who"
    done
    require_unreadable "$db" required "$who"
    require_unreadable "$db-wal" optional "$who"
    require_unreadable "$db-shm" optional "$who"
    require_unreadable "$evidence" required "$who"
    require_unreadable "$SUMMA_HOME/logs" optional "$who"
    local f
    for f in github_token gh-app.pem gh-app.json discord_auth.json openrouter_key config.json; do
      require_unreadable "$SUMMA_HOME/$f" optional "$who"
    done
    require_unreadable "$OWNER_HOME/.config/gh" optional "$who"
    require_unreadable "$OWNER_HOME/.config/gh/hosts.yml" optional "$who"
    # A broad sweep as well as the named paths.
    local leaked
    leaked="$("run_as_$who" find "$SUMMA_HOME" "$OWNER_HOME/.config/gh" "$(dirname "$db")" "$evidence" -readable 2>/dev/null | head -5 || true)"
    if [ -n "$leaked" ]; then printf '  READABLE by %s (BAD):\n%s\n' "$who" "$leaked" >&2; fail=1; fi
  done
  verify_cross_home_isolation
  check_toolchain
  if is_wsl && run_as_model test -r /mnt/c 2>/dev/null; then
    warn "$MODEL_USER can read /mnt/c (see the WSL warning above)."
  fi
  if is_wsl && run_as_review test -r /mnt/c 2>/dev/null; then
    warn "$REVIEW_USER can read /mnt/c (see the WSL warning above)."
  fi
  if is_wsl && run_as_validate test -r /mnt/c 2>/dev/null; then
    warn "$VALIDATE_USER can read /mnt/c (see the WSL warning above)."
  fi
  [ "$fail" -eq 0 ] || die "a dedicated user can read Box secrets (or a role home) or a secret path could not be evaluated; fix the lines above and re-run"

  cat <<NEXT

Done. Next steps (as the Box owner, not root):

  1. For any CLI whose login was not copied, log it in AS its role user (producer in $MODEL_HOME, reviewer in $REVIEW_HOME):
       sudo -u $MODEL_USER -H codex login
       sudo -u $REVIEW_USER -H claude     # /login
       sudo -u $MODEL_USER -H agy         # sign in
     The Copilot executor runs as $REVIEW_USER and needs its MCP server definition in $REVIEW_HOME/.copilot/mcp-config.json
     (offered above with the same content scan, or create it as $REVIEW_USER).
     Never log in as $VALIDATE_USER: it must hold no credentials (this script refuses if it finds any).
  2. Make sure the model binaries are executable by $MODEL_USER and $REVIEW_USER (not under $OWNER_HOME).
  3. Validation runs candidate code as $VALIDATE_USER with GOTOOLCHAIN=local. Install the Go toolchain this
     repository needs where $VALIDATE_USER can execute it (summa42 itself needs go1.27.x, e.g. under
     /usr/local/go1.27; /usr/bin/go is usually older) and list its bin directory:
       export SUMMA42_VALIDATION_TOOLCHAIN_DIRS=/usr/local/go1.27/bin
     Re-run this script with --toolchain-dirs /usr/local/go1.27/bin to check that \`go version\` works as $VALIDATE_USER.
     The Go build cache is per run, inside the scratch directory, and removed with it.
  4. Tell Summa42 which users to use:
       export SUMMA42_MODEL_USER=$MODEL_USER
       export SUMMA42_REVIEW_MODEL_USER=$REVIEW_USER
       export SUMMA42_VALIDATION_USER=$VALIDATE_USER
     or pass --model-user=$MODEL_USER, --review-model-user=$REVIEW_USER and --validation-user=$VALIDATE_USER to summa42-box.
  5. Verify on this real host: see "Verify isolation on a real host" in the README.
NEXT
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
