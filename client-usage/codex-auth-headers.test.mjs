import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, chmod, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';

// Only the inference key may leave the private credential file.
test('header helper emits only inference authorization and fails closed', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'proxy-usage-helper-test-'));
  try {
    const file = join(dir, 'credentials.json');
    await writeFile(file, JSON.stringify({ inference_key: 'fixture-inference', management_key: 'DO-NOT-EMIT', telegram_token: 'DO-NOT-EMIT' }), { mode: 0o600 });
    const run = () => spawnSync(process.execPath, [new URL('./codex-auth-headers.mjs', import.meta.url).pathname, file], { encoding: 'utf8' });
    const good = run();
    assert.equal(good.status, 0);
    assert.deepEqual(JSON.parse(good.stdout), { Authorization: 'Bearer fixture-inference' });
    assert.equal(good.stderr, '');
    await chmod(file, 0o644);
    const unsafe = run();
    assert.notEqual(unsafe.status, 0);
    assert.equal(unsafe.stdout, '');
    assert.ok(!unsafe.stderr.includes('DO-NOT-EMIT'));
    await chmod(file, 0o600);
    await writeFile(file, JSON.stringify({ inference_key: 'bad\nkey', management_key: 'DO-NOT-EMIT' }));
    const malformed = run();
    assert.notEqual(malformed.status, 0);
    assert.equal(malformed.stdout, '');
    assert.ok(!malformed.stderr.includes('DO-NOT-EMIT'));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
