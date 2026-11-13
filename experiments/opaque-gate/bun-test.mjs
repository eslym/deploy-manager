import { client, ready } from '@serenity-kit/opaque';
import { spawn, spawnSync } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';

await ready;
const esm = await Bun.file('node_modules/@serenity-kit/opaque/esm/index.js').arrayBuffer();
const cjs = await Bun.file('node_modules/@serenity-kit/opaque/cjs/index.js').arrayBuffer();
assert.equal(createHash('sha256').update(new Uint8Array(esm)).digest('hex'), '1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52');
assert.equal(createHash('sha256').update(new Uint8Array(cjs)).digest('hex'), '903c6cfa9ee052a04102e21ea4f2f36da753572618765bb21a1a5cccd1b00442');
const binary = '/tmp/opencode/opaque-gate-server';
const build = spawnSync('go', ['build', '-o', binary, '.'], { encoding: 'utf8' });
if (build.status !== 0) throw new Error(build.stderr);
const server = spawn(binary, [], { stdio: ['ignore', 'ignore', 'pipe'] });
let serverLog = '';
server.stderr.on('data', data => serverLog += data);
try {
  for (let i = 0; i < 100; i++) { try { await fetch('http://127.0.0.1:4179/'); break; } catch { await delay(100); } }
  const traffic = [];
  const post = async (path, body) => {
    traffic.push(JSON.stringify(body));
    const response = await fetch(`http://127.0.0.1:4179${path}`, { method: 'POST', headers: {'content-type':'application/json'}, body: JSON.stringify(body) });
    return {status: response.status, ...(response.status < 300 && response.status !== 204 ? await response.json() : {})};
  };
  const password = 'correct horse battery staple';
  const keyStretching = {'argon2id-custom': {iterations:1,memory:65536,parallelism:4}};
  const registration = client.startRegistration({password});
  const response = await post('/register/start', {value:registration.registrationRequest});
  const record = client.finishRegistration({password,registrationResponse:response.value,clientRegistrationState:registration.clientRegistrationState,keyStretching});
  assert.equal((await post('/register/finish',{value:record.registrationRecord})).status,204);
  const start = client.startLogin({password});
  const loginResponse = await post('/login/start',{value:start.startLoginRequest});
  const finish = client.finishLogin({password,loginResponse:loginResponse.value,clientLoginState:start.clientLoginState,keyStretching});
  assert.ok(finish);
  assert.equal((await post('/login/finish',{value:finish.finishLoginRequest,tx:loginResponse.tx})).status,204);
  const wrong = client.startLogin({password:'wrong password'});
  const wrongResponse = await post('/login/start',{value:wrong.startLoginRequest});
  assert.equal(client.finishLogin({password:'wrong password',loginResponse:wrongResponse.value,clientLoginState:wrong.clientLoginState,keyStretching}),undefined);
  assert.equal((await post('/login/start',{value:'AA'})).status,400);
  assert.equal((await post('/login/start',{value:''})).status,400);
  const tamperStart = client.startLogin({password});
  const tamperResponse = await post('/login/start',{value:tamperStart.startLoginRequest});
  const tamperFinish = client.finishLogin({password,loginResponse:tamperResponse.value,clientLoginState:tamperStart.clientLoginState,keyStretching});
  const tamperedBytes = Buffer.from(tamperFinish.finishLoginRequest.replace(/-/g,'+').replace(/_/g,'/'),'base64');
  tamperedBytes[tamperedBytes.length-1] ^= 1;
  const tamperedRequest = tamperedBytes.toString('base64url');
  assert.equal((await post('/login/finish',{value:tamperedRequest,tx:tamperResponse.tx})).status,400);
  for (const body of traffic) {
    assert.equal(body.includes(password),false);
    assert.equal(body.includes('wrong password'),false);
    const keys = Object.keys(JSON.parse(body));
    assert.ok(keys.every(key => key === 'value' || key === 'tx'), `unexpected field(s): ${keys}`);
  }
  console.log(JSON.stringify({artifact:{esmBytes:esm.byteLength,cjsBytes:cjs.byteLength,esmSha256:createHash('sha256').update(new Uint8Array(esm)).digest('hex'),cjsSha256:createHash('sha256').update(new Uint8Array(cjs)).digest('hex')},registrationLogin:true,wrongPasswordRejected:true,malformedAndTruncatedRejected:true,tamperedKe3Rejected:true,secretFieldsAbsent:true,serverLog},null,2));
} finally {
  server.kill('SIGTERM');
  await new Promise(resolve => server.once('exit',resolve));
}
