#!/usr/bin/env node
// Codex reads this stdout as a private HTTP header map, not as a tool result.
// Keep management and provider credentials out of MCP configuration and output.
import { constants } from 'node:fs';
import { open } from 'node:fs/promises';

let file;
try {
  if (process.argv.length !== 3) throw new Error('invalid arguments');
  file = await open(process.argv[2], constants.O_RDONLY | constants.O_NOFOLLOW);
  const stat = await file.stat();
  if (!stat.isFile() || stat.size > 65536 || (stat.mode & 0o077) !== 0 || stat.uid !== process.getuid()) throw new Error('unsafe file');
  const config = JSON.parse(await file.readFile('utf8'));
  const key = config.inference_key;
  if (typeof key !== 'string' || key.length === 0 || key.length > 4096 || !/^[\x21-\x7e]+$/.test(key)) throw new Error('invalid key');
  process.stdout.write(JSON.stringify({ Authorization: `Bearer ${key}` }));
} catch {
  process.stderr.write('Proxy usage authentication is unavailable; check the private inference credential file and its permissions.\n');
  process.exitCode = 1;
} finally {
  await file?.close();
}
