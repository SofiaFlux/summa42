#!/usr/bin/env bash
# One-time setup of the dedicated OS user that Summa42 model processes run as.
#
# Run as:  sudo scripts/setup-model-user.sh
#
# Idempotent. It prints every step. It creates the system user summa42-model
# (own home, no login shell, no extra groups), authorises the invoking user to
# run commands as it (and only as it) via sudoers, locks the invoking user's
# ~/.summa42 and ~/.config/gh to mode 0700, and then PROVES the model user cannot
# read the Box secrets. It exits non-zero if any secret is readable.
set -euo pipefail

ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --yes|--default-approve) ASSUME_YES=1 ;;
    *) printf 'usage: %s [--yes|--default-approve]\n' "$0" >&2; exit 2 ;;
  esac
done
MODEL_USER="${SUMMA42_MODEL_USER:-summa42-model}"
SUDOERS_FILE="/etc/sudoers.d/summa42-model"

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run with sudo: sudo $0"
OWNER="${SUDO_USER:-}"
[ -n "$OWNER" ] && [ "$OWNER" != root ] || die "run via sudo from the Box owner's account (SUDO_USER is unset or root)"
[ "$OWNER" != "$MODEL_USER" ] || die "the model user must differ from the Box owner"
OWNER_HOME="$(getent passwd "$OWNER" | cut -d: -f6)"
[ -d "$OWNER_HOME" ] || die "cannot find the home directory of $OWNER"
SUMMA_HOME="${SUMMA42_HOME:-$OWNER_HOME/.summa42}"

# 1. The user.
if id "$MODEL_USER" >/dev/null 2>&1; then
  say "user $MODEL_USER already exists; leaving it as is"
else
  say "creating system user $MODEL_USER (home /var/lib/$MODEL_USER, no login shell, no groups)"
  useradd --system --create-home --home-dir "/var/lib/$MODEL_USER" \
    --shell /usr/sbin/nologin --user-group "$MODEL_USER"
fi
MODEL_HOME="$(getent passwd "$MODEL_USER" | cut -d: -f6)"
[ "$(id -u "$MODEL_USER")" -ne 0 ] || die "$MODEL_USER resolved to uid 0"
say "ensuring $MODEL_HOME and $MODEL_HOME/tmp are private to $MODEL_USER (0700)"
install -d -m 0700 -o "$MODEL_USER" -g "$MODEL_USER" "$MODEL_HOME" "$MODEL_HOME/tmp"
extra_groups="$(id -nG "$MODEL_USER" | tr ' ' '\n' | grep -vx "$MODEL_USER" || true)"
[ -z "$extra_groups" ] || die "$MODEL_USER belongs to extra groups ($extra_groups); remove them"

# 2. sudoers drop-in, validated before it is installed.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf '%s ALL=(%s) NOPASSWD: ALL\n' "$OWNER" "$MODEL_USER" >"$tmp"
visudo -cf "$tmp" >/dev/null || die "generated sudoers entry failed visudo -cf"
if [ -f "$SUDOERS_FILE" ] && cmp -s "$tmp" "$SUDOERS_FILE"; then
  say "$SUDOERS_FILE already up to date; leaving it as is"
else
  say "writing $SUDOERS_FILE: $OWNER may run commands as $MODEL_USER without a password"
  install -m 0440 -o root -g root "$tmp" "$SUDOERS_FILE.new"
  visudo -cf "$SUDOERS_FILE.new" >/dev/null || { rm -f "$SUDOERS_FILE.new"; die "sudoers file failed validation and was not installed"; }
  mv -f "$SUDOERS_FILE.new" "$SUDOERS_FILE"
fi

# 3. Lock the Box owner's secret directories.
for dir in "$SUMMA_HOME" "$OWNER_HOME/.config/gh"; do
  if [ -e "$dir" ]; then
    say "chmod 0700 $dir"
    chmod 0700 "$dir"
  else
    say "$dir does not exist; skipping"
  fi
done

# 3b. Optionally copy the invoking user's model-CLI logins so no re-login is
# needed. Each CLI is asked about separately (y/yes copies); --yes approves all.
# Without a TTY and without --yes nothing is copied. ~/.config/gh, ~/.summa42
# and gh tokens are never offered, even with --yes. Re-running refreshes copies.
copied=()
skipped=()
approve() {
  [ "$ASSUME_YES" -eq 1 ] && return 0
  [ -t 0 ] || return 1
  local answer
  printf '  %s %s -> %s ? [y/N] ' "$3" "$1" "$2"
  read -r answer || return 1
  case "$answer" in y|Y|yes|YES|Yes) return 0 ;; *) return 1 ;; esac
}
say "CLI auth import (asks per CLI; --yes approves all; nothing is copied without a TTY or --yes)"
for rel in .claude .claude.json .codex .gemini .config/antigravity .antigravity .agy .config/agy .copilot .config/github-copilot; do
  src="$OWNER_HOME/$rel"
  [ -e "$src" ] || continue
  real="$(readlink -f "$src")"
  case "$real" in
    "$SUMMA_HOME"|"$SUMMA_HOME"/*|"$OWNER_HOME/.config/gh"|"$OWNER_HOME/.config/gh"/*)
      say "  not offering $src: it resolves into a Box secret location"; continue ;;
  esac
  dest="$MODEL_HOME/$rel"
  case "$rel" in
    .copilot|.config/github-copilot)
      printf 'WARNING: %s holds a GitHub-issued Copilot credential; copying it hands the model a GitHub credential.\n' "$rel" >&2 ;;
  esac
  verb=copy
  [ -e "$dest" ] && verb=refresh
  if ! approve "$src" "$dest" "$verb"; then
    say "  skipped $rel (any existing copy is left untouched)"
    skipped+=("$rel")
    continue
  fi
  install -d -m 0700 -o "$MODEL_USER" -g "$MODEL_USER" "$(dirname "$dest")"
  # Copy beside the destination, fix owner/modes, then swap in with rename, so
  # a half-copied state never replaces a working copy.
  staging="$dest.new.$$"
  rm -rf "$staging"
  if ! cp -a "$src" "$staging"; then
    rm -rf "$staging"; die "copying $src failed; the existing copy (if any) was left untouched"
  fi
  chown -R "$MODEL_USER:$MODEL_USER" "$staging"
  if [ -d "$staging" ]; then chmod -R u+rwX,go-rwx "$staging"; else chmod 0600 "$staging"; fi
  if [ -e "$dest" ]; then mv "$dest" "$dest.old.$$"; fi
  mv "$staging" "$dest"
  rm -rf "$dest.old.$$"
  chown "$MODEL_USER:$MODEL_USER" "$(dirname "$dest")"
  say "  done: $verb $rel"
  copied+=("$dest")
done
if [ "${#skipped[@]}" -gt 0 ]; then
  say "skipped (log in as $MODEL_USER instead, or re-run with --yes): ${skipped[*]}"
fi
if [ ! -d "$OWNER_HOME/.copilot" ] && [ ! -d "$OWNER_HOME/.config/github-copilot" ]; then
  say "no Copilot auth directory found; if Copilot only works through the gh token it is NOT copied."
  say "Copilot then needs a separate login as $MODEL_USER, which hands the model a GitHub credential."
fi
if [ "${#copied[@]}" -gt 0 ]; then
  leaked="$(find "${copied[@]}" \( -name owner.key -o -name github_token -o -name gh-app.pem -o -name discord_auth.json -o -name hosts.yml -o -name 'summa42.db*' \) 2>/dev/null || true)"
  if [ -n "$leaked" ]; then
    printf 'copied auth contains Box secret files:\n%s\n' "$leaked" >&2
    die "refusing to continue; remove them from $MODEL_HOME"
  fi
fi

# 4. Prove the model user cannot read the secrets.
say "verifying $MODEL_USER cannot read the Box secrets"
fail=0
check() {
  local path="$1"
  [ -e "$path" ] || return 0
  if sudo -n -u "$MODEL_USER" -- test -r "$path"; then
    printf '  READABLE (BAD): %s\n' "$path" >&2
    fail=1
  else
    printf '  ok, not readable: %s\n' "$path"
  fi
}
shopt -s nullglob
for p in "$SUMMA_HOME" "$SUMMA_HOME"/keys "$SUMMA_HOME"/keys/* "$SUMMA_HOME/github_token" \
         "$SUMMA_HOME/gh-app.pem" "$SUMMA_HOME/discord_auth.json" "$SUMMA_HOME"/state \
         "$SUMMA_HOME"/state/*.db "$SUMMA_HOME"/state/*.db-* \
         "$OWNER_HOME/.config/gh" "$OWNER_HOME/.config/gh"/*; do
  check "$p"
done
[ "$fail" -eq 0 ] || die "the model user can read Box secrets; fix the permissions above and re-run"

cat <<NEXT

Done. Next steps (as the Box owner, not root):

  1. For any CLI whose login was not copied, log it in AS the model user (credentials live in $MODEL_HOME):
       sudo -u $MODEL_USER -H codex login
       sudo -u $MODEL_USER -H agy        # sign in
       sudo -u $MODEL_USER -H claude     # /login
  2. Make sure the model binaries are executable by $MODEL_USER (not under $OWNER_HOME).
  3. Tell Summa42 which user to use:
       export SUMMA42_MODEL_USER=$MODEL_USER
     or pass --model-user=$MODEL_USER to summa42-box.
NEXT
