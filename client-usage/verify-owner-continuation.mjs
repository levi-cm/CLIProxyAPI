// Live verification only. Keep the generated encrypted conversation file private.
// The temporary SOCKS5 relay is loopback-only and connects solely to this proxy.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import net from 'node:net';
import { randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';

const target = '100.82.251.30';
const port = 8317;
const [phase, statePath] = process.argv.slice(2);
assert.ok(['seed', 'resume'].includes(phase) && statePath, 'Usage: seed|resume private-state-file');
const config = JSON.parse(fs.readFileSync(new URL('../telegram-companion/private/config.json', import.meta.url)));
assert.ok(typeof config.inference_key === 'string' && !/[\r\n]/.test(config.inference_key));

async function bytes(socket, length) {
  for (;;) {
    const value = socket.read(length);
    if (value) return value;
    if (socket.destroyed || socket.readableEnded) throw new Error('SOCKS connection ended');
    await new Promise((resolve, reject) => {
      const ready = () => { cleanup(); resolve(); };
      const fail = () => { cleanup(); reject(new Error('SOCKS connection failed')); };
      const cleanup = () => { socket.off('readable', ready); socket.off('error', fail); socket.off('end', fail); };
      socket.once('readable', ready); socket.once('error', fail); socket.once('end', fail);
    });
  }
}

const sockets = new Set();
let connections = 0;
const relay = net.createServer(async socket => {
  sockets.add(socket); socket.on('close', () => sockets.delete(socket));
  socket.on('error', () => {});
  try {
    const hello = await bytes(socket, 2);
    assert.equal(hello[0], 5);
    const methods = await bytes(socket, hello[1]);
    assert.ok(methods.includes(0));
    socket.write(Buffer.from([5, 0]));
    const request = await bytes(socket, 4);
    assert.equal(request[0], 5); assert.equal(request[1], 1);
    let host;
    if (request[3] === 1) host = [...await bytes(socket, 4)].join('.');
    else if (request[3] === 3) host = (await bytes(socket, (await bytes(socket, 1))[0])).toString();
    else throw new Error('Unsupported SOCKS address');
    const destinationPort = (await bytes(socket, 2)).readUInt16BE();
    assert.equal(host, target); assert.equal(destinationPort, port);
    const upstream = net.connect(port, target);
    sockets.add(upstream); upstream.on('close', () => sockets.delete(upstream));
    upstream.on('error', () => socket.destroy());
    await new Promise((resolve, reject) => { upstream.once('connect', resolve); upstream.once('error', reject); });
    connections++;
    socket.write(Buffer.from([5, 0, 0, 1, 127, 0, 0, 1, 0, 0]));
    socket.pipe(upstream); upstream.pipe(socket);
    socket.on('close', () => upstream.destroy()); upstream.on('close', () => socket.destroy());
  } catch { socket.destroy(); }
});
await new Promise(resolve => relay.listen(0, '127.0.0.1', resolve));
const socks = `127.0.0.1:${relay.address().port}`;
const quoted = value => '"' + value.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n') + '"';

async function request(path, body, headers = {}) {
  const lines = ['url = '+quoted(`http://${target}:${port}${path}`), 'header = '+quoted('Authorization: Bearer '+config.inference_key)];
  if (body !== undefined) lines.push('request = "POST"', 'header = "Content-Type: application/json"', 'data = '+quoted(JSON.stringify(body)));
  for (const [key, value] of Object.entries(headers)) lines.push('header = '+quoted(`${key}: ${value}`));
  const result = await new Promise((resolve, reject) => {
    const child = spawn('curl', ['--silent','--show-error','--noproxy','','--socks5-hostname',socks,'--config','-','--write-out','\n%{http_code}'], {stdio:['pipe','pipe','pipe']});
    let output = ''; child.stdout.on('data', b => { output += b; });
    child.stderr.resume(); child.on('error', () => reject(new Error('Cannot start curl')));
    child.on('close', code => { if (code !== 0) reject(new Error('SOCKS HTTP request failed')); else resolve(output); });
    child.stdin.end(lines.join('\n')+'\n');
  });
  const separator = result.lastIndexOf('\n');
  const status = Number(result.slice(separator+1));
  const json = JSON.parse(result.slice(0, separator));
  return {status, json};
}

async function report(thread) {
  const r = await request('/v1/account-policy/usage?session_id='+encodeURIComponent(thread));
  assert.equal(r.status, 200, 'usage read failed');
  assert.ok(r.json.last_successful_account, 'no successful owner observed');
  return r.json.last_successful_account;
}

try {
  if (phase === 'seed') {
    const thread = randomUUID();
    const model = 'gpt-6.1-sol';
    const input = [{role:'user', content:'Find the smallest positive integer x such that x modulo 17 is 5, x modulo 19 is 7, and x modulo 23 is 11. Verify all three congruences. Reply with the number and a compact verification.'}];
    const r = await request('/v1/responses', {model, input, stream:false, store:false, reasoning:{effort:'high'}, include:['reasoning.encrypted_content']}, {Session_id:thread});
    assert.equal(r.status, 200, 'initial provider response failed');
    const encrypted = r.json.output?.filter(item => item.encrypted_content).length || 0;
    assert.ok(encrypted > 0, 'provider returned no actual encrypted reasoning; cannot prove continuation');
    const owner = await report(thread);
    const state = {thread, model, input:[...input,...r.json.output], owner:owner.credential_id, label:owner.label};
    fs.writeFileSync(statePath, JSON.stringify(state), {mode:0o600, flag:'wx'});
    console.log(JSON.stringify({phase, account:owner.label, encrypted_items:encrypted, socks_connections:connections, binding_seeded:true}));
  } else {
    const file = fs.openSync(statePath, fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW);
    const stat = fs.fstatSync(file);
    assert.ok(stat.isFile() && (stat.mode & 0o077) === 0 && stat.size < 1024*1024, 'state file must be private and bounded');
    const state = JSON.parse(fs.readFileSync(file,'utf8')); fs.closeSync(file);
    const body = {model:state.model, input:[...state.input,{role:'user',content:'Continue: add one to that result. Reply only with the answer.'}], stream:false, store:false, include:['reasoning.encrypted_content']};
    const resumed = await request('/v1/responses', body, {Session_id:state.thread});
    assert.equal(resumed.status, 200, 'encrypted resume failed after restart');
    const owner = await report(state.thread); assert.equal(owner.credential_id,state.owner,'resume changed account');
    const child = randomUUID();
    const forked = await request('/v1/responses', body, {Session_id:child, 'X-Openai-Subagent':'true', 'X-Codex-Parent-Thread-Id':state.thread});
    assert.equal(forked.status, 200, 'forked agent failed to inherit verified owner');
    const childOwner = await report(child); assert.equal(childOwner.credential_id,state.owner,'fork changed account');
    const unknown = await request('/v1/responses', body, {Session_id:randomUUID()});
    assert.equal(unknown.status,409,'unowned encrypted continuation was accepted');
    console.log(JSON.stringify({phase,account:owner.label,resume_status:resumed.status,fork_status:forked.status,unknown_status:unknown.status,socks_connections:connections,same_owner:true}));
  }
} finally {
  for (const socket of sockets) socket.destroy();
  await new Promise(resolve => relay.close(resolve));
}
