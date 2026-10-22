'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { checkModelUser, modelLaunchArgv, spawnTimeoutMs } = require('./model-launch.js');

const self = { uid: 1000, gid: 1000 };
const entry = (uid, gid, home) => ['m', 'x', String(uid), String(gid), '', home, '/usr/sbin/nologin'];

test('accepts a dedicated user', () => {
  assert.deepStrictEqual(checkModelUser('summa42-model', entry(998, 998, '/var/lib/m'), self), { name: 'summa42-model', home: '/var/lib/m' });
});
test('rejects an alias that shares the Box uid under another name', () => {
  assert.ok(checkModelUser('alias', entry(1000, 1001, '/home/x'), self).error);
});
test('rejects a shared primary gid, root, bad names and relative homes', () => {
  assert.ok(checkModelUser('m', entry(998, 1000, '/var/lib/m'), self).error);
  assert.ok(checkModelUser('root', entry(0, 0, '/root'), self).error);
  assert.ok(checkModelUser('', [], self).error);
  assert.ok(checkModelUser('a b;ALL', entry(998, 998, '/h'), self).error);
  assert.ok(checkModelUser('m', entry(998, 998, 'rel'), self).error);
  assert.ok(checkModelUser('m', [], self).error);
});
test('argv carries the model-side timeout and no env values', () => {
  const argv = modelLaunchArgv({ name: 'm', home: '/var/lib/m' }, 'agy', ['--flag'], 45000, 'C.UTF-8');
  assert.deepStrictEqual(argv.slice(0, 7), ['-n', '-u', 'm', '--', '/usr/bin/timeout', '--kill-after=15s', '45s']);
  assert.strictEqual(argv[argv.length - 2], 'agy');
  assert.ok(!argv.some((a) => /token|secret/i.test(a)));
  assert.strictEqual(modelLaunchArgv({ name: 'm', home: '/h' }, 'x', [], 100)[6], '1s');
});
test('spawn backstop exceeds the model-side timeout plus kill-after', () => {
  assert.ok(spawnTimeoutMs(45000) >= 45000 + 15000);
});
