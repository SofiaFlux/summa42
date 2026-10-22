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
say "writing $SUDOERS_FILE: $OWNER may run commands as $MODEL_USER without a password"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
printf '%s ALL=(%s) NOPASSWD: ALL\n' "$OWNER" "$MODEL_USER" >"$tmp"
visudo -cf "$tmp" >/dev/null || die "generated sudoers entry failed visudo -cf"
install -m 0440 -o root -g root "$tmp" "$SUDOERS_FILE"
visudo -cf "$SUDOERS_FILE" >/dev/null || { rm -f "$SUDOERS_FILE"; die "installed sudoers file failed validation and was removed"; }

# 3. Lock the Box owner's secret directories.
for dir in "$SUMMA_HOME" "$OWNER_HOME/.config/gh"; do
  if [ -e "$dir" ]; then
    say "chmod 0700 $dir"
    chmod 0700 "$dir"
  else
    say "$dir does not exist; skipping"
  fi
done

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

  1. Log each model CLI in AS the model user, so its credentials live in $MODEL_HOME:
       sudo -u $MODEL_USER -H codex login
       sudo -u $MODEL_USER -H agy        # sign in
       sudo -u $MODEL_USER -H claude     # /login
  2. Make sure the model binaries are executable by $MODEL_USER (not under $OWNER_HOME).
  3. Tell Summa42 which user to use:
       export SUMMA42_MODEL_USER=$MODEL_USER
     or pass --model-user=$MODEL_USER to summa42-box.
NEXT
