#!/usr/bin/env bash
# One-time setup of the dedicated OS user that Summa42 model processes run as.
#
# Run as:  sudo scripts/setup-model-user.sh [--yes|--default-approve]
#            [--summa-home DIR] [--db FILE] [--evidence DIR]
#
# Idempotent. It prints every step. It creates the system user summa42-model
# (own home, no login shell, no extra groups), authorises the invoking user to
# run commands as it (and only as it) via sudoers, locks the invoking user's
# ~/.summa42 and ~/.config/gh to mode 0700, and then PROVES the model user cannot
# read the Box secrets. It exits non-zero if any secret is readable or if a
# configured secret path cannot be evaluated.
#
# Trust boundary: the model user owns its home and a prompt-injected model can
# plant symlinks there. Root therefore never reads, writes, chmods or chowns
# anything inside the model home after `useradd`: every file operation there is
# done BY the model user (run_as_model). Root only creates the user and writes
# /etc/sudoers.d.
set -euo pipefail

MODEL_USER="${SUMMA42_MODEL_USER:-summa42-model}"
SUDOERS_FILE="/etc/sudoers.d/summa42-model"
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

# Run a command as the model user, with a clean environment and cwd /. Root
# never touches the model home itself; this is the only way it is reached.
run_as_model() {
  (
    cd /
    if command -v runuser >/dev/null 2>&1; then
      exec runuser -u "$MODEL_USER" -- env -i HOME="$MODEL_HOME" PATH=/usr/local/bin:/usr/bin:/bin "$@"
    else
      exec sudo -n -u "$MODEL_USER" -- env -i HOME="$MODEL_HOME" PATH=/usr/local/bin:/usr/bin:/bin "$@"
    fi
  )
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

# copy_file SRCROOT REL: stream SRCROOT/REL into the model home as the model user.
copy_file() {
  tar -C "$1" -cf - -- "$2" | run_as_model sh -c "$MODEL_INSTALL_SH" sh "$2"
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
    gemini)  echo ".gemini/oauth_creds.json .gemini/google_accounts.json .gemini/installation_id .gemini/settings.json" ;;
    copilot) echo ".copilot/config.json .copilot/mcp-config.json .config/github-copilot/apps.json .config/github-copilot/hosts.json" ;;
  esac
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

# import_cli NAME: copy one CLI's auth files after the content scan. Returns 2
# when a token pattern was found, 3 when any step failed. Called as
# `import_cli x || rc=$?`, which switches errexit off for the whole body, so
# every step here ends in an explicit `|| return 3`.
import_cli() {
  local rc=0
  stagedir="$(mktemp -d)" || return 3
  if [ -z "$stagedir" ] || [ "$stagedir" = / ] || [ ! -d "$stagedir" ]; then
    printf 'cannot create a staging directory\n' >&2; return 3
  fi
  _import_cli "$1" || rc=$?
  rm -rf -- "$stagedir"
  return "$rc"
}

_import_cli() {
  local name="$1" rel files=() mode=default
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
  local verb=copy dest_exists=0
  for rel in "${files[@]}"; do
    if run_as_model test -e "$MODEL_HOME/$rel"; then dest_exists=1; fi
  done
  [ "$dest_exists" -eq 1 ] && verb=refresh
  if [ "$name" = copilot ]; then
    warn "copilot's login is a GitHub OAuth credential; copying it hands the model a GitHub credential."
    if [ "$ASSUME_YES" -eq 1 ]; then
      say "  skipped copilot: never copied under --yes; re-run without --yes to be asked"
      skipped+=("$name"); return 0
    fi
    ask "$verb $name auth (${files[*]})" "$MODEL_HOME" || { say "  skipped $name"; skipped+=("$name"); return 0; }
  else
    approve "$verb $name auth (${files[*]})" "$MODEL_HOME" || { say "  skipped $name (any existing copy is left untouched)"; skipped+=("$name"); return 0; }
  fi
  local n=0
  for rel in "${files[@]}"; do
    if [[ " ${staged[*]:-} " == *" $rel "* ]]; then
      copy_file "$stagedir" "$rel" || { printf 'copying %s failed\n' "$rel" >&2; return 3; }
    else
      copy_file "$OWNER_HOME" "$rel" || { printf 'copying %s failed\n' "$rel" >&2; return 3; }
    fi
    n=$((n + 1))
  done
  say "  done: $verb $name ($n file(s))"
  copied+=("$name")
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
# require_unreadable PATH KIND: KIND is "required" (must exist) or "optional".
require_unreadable() {
  local path="$1" kind="$2"
  if [ ! -e "$path" ]; then
    if [ "$kind" = required ]; then
      printf '  MISSING (cannot evaluate): %s\n' "$path" >&2; fail=1
    fi
    return 0
  fi
  if run_as_model test -r "$path"; then
    printf '  READABLE (BAD): %s\n' "$path" >&2; fail=1
  else
    printf '  ok, not readable: %s\n' "$path"
  fi
}

main() {
  ASSUME_YES=0
  local arg_home="" arg_db="" arg_evidence=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --yes|--default-approve) ASSUME_YES=1 ;;
      --summa-home) arg_home="${2:?}"; shift ;;
      --db) arg_db="${2:?}"; shift ;;
      --evidence) arg_evidence="${2:?}"; shift ;;
      *) printf 'usage: %s [--yes|--default-approve] [--summa-home DIR] [--db FILE] [--evidence DIR]\n' "$0" >&2; exit 2 ;;
    esac
    shift
  done
  valid_model_user "$MODEL_USER" || die "SUMMA42_MODEL_USER=$MODEL_USER is not a valid system user name (^[a-z_][a-z0-9_-]{0,31}\$)"

  [ "$(id -u)" -eq 0 ] || die "run with sudo: sudo $0"
  OWNER="${SUDO_USER:-}"
  [ -n "$OWNER" ] && [ "$OWNER" != root ] || die "run via sudo from the Box owner's account (SUDO_USER is unset or root)"
  valid_owner_name "$OWNER" || die "unusual owner account name: $OWNER"
  [ "$OWNER" != "$MODEL_USER" ] || die "the model user must differ from the Box owner"
  OWNER_HOME="$(getent passwd "$OWNER" | cut -d: -f6)"
  [ -d "$OWNER_HOME" ] || die "cannot find the home directory of $OWNER"
  SUMMA_HOME="${arg_home:-${SUMMA42_HOME:-$OWNER_HOME/.summa42}}"
  [ -d "$SUMMA_HOME" ] || die "Summa42 home $SUMMA_HOME does not exist; pass --summa-home DIR (sudo strips SUMMA42_HOME from the environment)"

  if is_wsl; then wsl_warning; fi

  # 1. The user. Root creates it and its home, nothing more.
  if id "$MODEL_USER" >/dev/null 2>&1; then
    local shell uid uid_min
    shell="$(getent passwd "$MODEL_USER" | cut -d: -f7)"
    uid="$(id -u "$MODEL_USER")"
    uid_min="$(awk '$1=="UID_MIN"{print $2}' /etc/login.defs 2>/dev/null || true)"
    case "$shell" in */nologin|*/false) ;; *) die "existing user $MODEL_USER has login shell $shell; refusing to use a real account" ;; esac
    [ "$uid" -lt "${uid_min:-1000}" ] || die "existing user $MODEL_USER (uid $uid) is not a system account; refusing to use it"
    say "user $MODEL_USER already exists; leaving it as is"
  else
    say "creating system user $MODEL_USER (home /var/lib/$MODEL_USER, no login shell, no groups)"
    useradd --system --create-home --home-dir "/var/lib/$MODEL_USER" \
      --shell /usr/sbin/nologin --user-group "$MODEL_USER"
  fi
  MODEL_HOME="$(getent passwd "$MODEL_USER" | cut -d: -f6)"
  [ "$(id -u "$MODEL_USER")" -ne 0 ] || die "$MODEL_USER resolved to uid 0"
  say "ensuring $MODEL_HOME and $MODEL_HOME/tmp are private to $MODEL_USER (0700), as $MODEL_USER"
  run_as_model chmod 0700 "$MODEL_HOME"
  run_as_model mkdir -p -m 0700 "$MODEL_HOME/tmp"
  local extra_groups
  extra_groups="$(id -nG "$MODEL_USER" | tr ' ' '\n' | grep -vx "$MODEL_USER" || true)"
  [ -z "$extra_groups" ] || die "$MODEL_USER belongs to extra groups ($extra_groups); remove them"

  # 2. sudoers drop-in: validated as a fragment, installed, then the whole
  # configuration is re-checked and the drop-in rolled back if that fails.
  local tmp backup
  tmp="$(mktemp)"; backup="$(mktemp)"
  trap 'rm -f "$tmp" "$backup"' EXIT
  sudoers_content "$OWNER" "$MODEL_USER" >"$tmp"
  visudo -cf "$tmp" >/dev/null || die "generated sudoers entry failed visudo -cf"
  if [ -f "$SUDOERS_FILE" ] && cmp -s "$tmp" "$SUDOERS_FILE"; then
    say "$SUDOERS_FILE already up to date; leaving it as is"
  else
    local had_old=0
    if [ -f "$SUDOERS_FILE" ]; then cp -p -- "$SUDOERS_FILE" "$backup"; had_old=1; fi
    say "writing $SUDOERS_FILE: $OWNER may run commands as $MODEL_USER without a password (sudo does not log them)"
    install -m 0440 -o root -g root "$tmp" "$SUDOERS_FILE.new"
    mv -f "$SUDOERS_FILE.new" "$SUDOERS_FILE"
    if ! visudo -c >/dev/null; then
      if [ "$had_old" -eq 1 ]; then mv -f "$backup" "$SUDOERS_FILE"; else rm -f "$SUDOERS_FILE"; fi
      die "full sudoers configuration failed visudo -c with the new drop-in; rolled it back"
    fi
  fi

  # 3. Lock the Box owner's secret directories (never through a symlink).
  chmod_owner_dir "$SUMMA_HOME"
  chmod_owner_dir "$OWNER_HOME/.config/gh"

  # 3b. Optionally copy the minimal auth files of the invoking user's model CLIs.
  copied=(); skipped=()
  say "CLI auth import (asks per CLI; --yes approves all except Copilot; nothing is copied without a TTY or --yes)"
  local cli rc
  for cli in claude codex gemini copilot; do
    rc=0; import_cli "$cli" || rc=$?
    case "$rc" in
      0) ;;
      2) die "refusing to continue; a token pattern was found in $cli's files and nothing from $cli was copied" ;;
      *) die "copying the $cli auth files failed (exit $rc); the copy may be incomplete; fix the error above and re-run" ;;
    esac
  done
  if [ "${#skipped[@]}" -gt 0 ]; then
    say "skipped (log in as $MODEL_USER instead, or re-run): ${skipped[*]}"
  fi

  # 4. Prove the model user cannot read the secrets. Every path is resolved as
  # the invoking user and the check fails if one cannot be evaluated.
  say "verifying $MODEL_USER cannot read the Box secrets"
  run_as_model test -r /etc/passwd || die "cannot run commands as $MODEL_USER; the readability check would be vacuous"
  local db evidence
  db="${arg_db:-$(owner_config_value database_path)}"
  evidence="${arg_evidence:-$(owner_config_value evidence_path)}"
  [ -n "$db" ] || die "cannot determine the state DB path (no database_path in $SUMMA_HOME/config.json); pass --db FILE"
  [ -n "$evidence" ] || die "cannot determine the evidence path; pass --evidence DIR"
  require_unreadable "$SUMMA_HOME" required
  require_unreadable "$SUMMA_HOME/keys" required
  require_unreadable "$db" required
  require_unreadable "$db-wal" optional
  require_unreadable "$db-shm" optional
  require_unreadable "$evidence" required
  require_unreadable "$SUMMA_HOME/logs" optional
  local f
  for f in github_token gh-app.pem gh-app.json discord_auth.json openrouter_key config.json; do
    require_unreadable "$SUMMA_HOME/$f" optional
  done
  require_unreadable "$OWNER_HOME/.config/gh" optional
  # A broad sweep as well as the named paths.
  local leaked
  leaked="$(run_as_model find "$SUMMA_HOME" "$OWNER_HOME/.config/gh" "$(dirname "$db")" "$evidence" -readable 2>/dev/null | head -5 || true)"
  if [ -n "$leaked" ]; then printf '  READABLE (BAD):\n%s\n' "$leaked" >&2; fail=1; fi
  if is_wsl && run_as_model test -r /mnt/c 2>/dev/null; then
    warn "$MODEL_USER can read /mnt/c (see the WSL warning above)."
  fi
  [ "$fail" -eq 0 ] || die "the model user can read Box secrets or a secret path could not be evaluated; fix the lines above and re-run"

  cat <<NEXT

Done. Next steps (as the Box owner, not root):

  1. For any CLI whose login was not copied, log it in AS the model user (credentials live in $MODEL_HOME):
       sudo -u $MODEL_USER -H codex login
       sudo -u $MODEL_USER -H agy        # sign in
       sudo -u $MODEL_USER -H claude     # /login
     The Copilot executor also needs its MCP server definition in $MODEL_HOME/.copilot/mcp-config.json
     (offered above with the same content scan, or create it as the model user).
  2. Make sure the model binaries are executable by $MODEL_USER (not under $OWNER_HOME).
  3. Tell Summa42 which user to use:
       export SUMMA42_MODEL_USER=$MODEL_USER
     or pass --model-user=$MODEL_USER to summa42-box.
NEXT
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
