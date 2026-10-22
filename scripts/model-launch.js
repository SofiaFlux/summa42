'use strict';
// Pure helpers for launching model CLIs as the dedicated model user. No I/O, so
// they are unit-tested (scripts/model-launch.test.js) without sudo.

const KILL_AFTER_SECONDS = 15;
const MODEL_PATH = '/usr/local/bin:/usr/bin:/bin';
const USER_RE = /^[a-z_][a-z0-9_-]*$/;

// fields: a `getent passwd` line split on ':'. self: {uid, gid} of the Box user.
// Returns {name, home} or {error}. Compares ids, not names: a passwd alias can
// share the Box's uid under another name.
function checkModelUser(name, fields, self) {
  if (!name) return { error: 'SUMMA42_MODEL_USER is not set' };
  if (!USER_RE.test(name)) return { error: `${name} is not a valid user name` };
  const home = fields[5];
  if (fields[2] === '0' || !home || !home.startsWith('/')) {
    return { error: `${name} does not exist, is root, or has no home directory` };
  }
  if (fields[2] === String(self.uid) || fields[3] === String(self.gid)) {
    return { error: `${name} shares the Box user's uid or primary group` };
  }
  return { name, home };
}

// The deadline is enforced on the model user's side by coreutils timeout (the
// Box uid cannot signal another uid's processes). No secret is passed on argv.
function modelLaunchArgv(model, bin, args, timeoutMs, lang) {
  const seconds = Math.max(1, Math.ceil(timeoutMs / 1000));
  return ['-n', '-u', model.name, '--', '/usr/bin/timeout', `--kill-after=${KILL_AFTER_SECONDS}s`, `${seconds}s`,
    '/usr/bin/env', '-i', '-C', model.home,
    `HOME=${model.home}`, `PATH=${MODEL_PATH}`, `LANG=${lang || 'C.UTF-8'}`, `TMPDIR=${model.home}/tmp`,
    bin, ...args];
}

// Backstop for spawnSync, above the model-side timeout plus kill-after, in case
// sudo itself hangs.
function spawnTimeoutMs(timeoutMs) {
  return timeoutMs + (KILL_AFTER_SECONDS + 5) * 1000;
}

module.exports = { checkModelUser, modelLaunchArgv, spawnTimeoutMs, KILL_AFTER_SECONDS };
