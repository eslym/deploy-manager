import { chromium } from 'playwright';
import { spawn, spawnSync } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';
import assert from 'node:assert/strict';

const binary = '/tmp/opencode/opaque-gate-server';
const build = spawnSync('go', ['build', '-o', binary, '.'], { encoding: 'utf8' });
if (build.status !== 0) throw new Error(build.stderr);
const server = spawn(binary, [], { stdio: ['ignore', 'pipe', 'pipe'] });
let serverLog = '';
server.stdout.on('data', x => serverLog += x); server.stderr.on('data', x => serverLog += x);
let browser;
const chromiumRss = () => spawnSync('ps', ['-eo','rss,args'], {encoding:'utf8'}).stdout.split('\n').filter(x => x.includes('chrome-headless-shell') && !x.includes('ps -eo')).map(x => Number(x.trim().split(/\s+/)[0]));
try {
  for (let i = 0; i < 100; i++) {
    try { await fetch('http://127.0.0.1:4179/'); break; } catch { await delay(100); }
  }
  browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  const requests = [];
  page.on('request', req => { if (req.method() === 'POST') requests.push(req.postData() || ''); });
  await page.goto('http://127.0.0.1:4179/');
  await page.waitForFunction(() => window.opaqueReady === true);
  const baselineRss = chromiumRss();
  const outcome = await page.evaluate(async () => {
    const c = window.opaqueClient;
    const password = 'correct horse battery staple';
    const profile = { 'argon2id-custom': { iterations: 1, memory: 65536, parallelism: 4 } };
    const post = async (path, body) => {
      const res = await fetch(path, { method: 'POST', headers: {'content-type':'application/json'}, body: JSON.stringify(body) });
      return { status: res.status, ...(res.status < 300 && res.status !== 204 ? await res.json() : {}) };
    };
    const regStartAt = performance.now();
    const startReg = c.startRegistration({password});
    const regRequestDone = performance.now();
    const regResponse = await post('/register/start', {value:startReg.registrationRequest});
    const regResponseAt = performance.now();
    const finishReg = c.finishRegistration({password, registrationResponse:regResponse.value, clientRegistrationState:startReg.clientRegistrationState, keyStretching:profile});
    const registrationDone = performance.now();
    await post('/register/finish', {value:finishReg.registrationRecord});
    const loginStartAt = performance.now();
    const clientLoginStart = performance.now();
    const loginStart = c.startLogin({password});
    const clientLoginStartDone = performance.now();
    const loginResponse = await post('/login/start', {value:loginStart.startLoginRequest});
    const loginResponseDone = performance.now();
    const loginFinish = c.finishLogin({password, loginResponse:loginResponse.value, clientLoginState:loginStart.clientLoginState, keyStretching:profile});
    const clientLoginFinishDone = performance.now();
    const loginDone = performance.now();
    if (loginResponse.status !== 200 || !loginFinish) return {diagnostic:{loginResponse,finish:loginFinish}};
    const loginFinishStatus = await post('/login/finish', {value:loginFinish.finishLoginRequest, tx:loginResponse.tx});
    const out = {registrationStatus:regResponse.status,registrationStored:true,loginFinishStatus,clientKeyProducedAndKe3Verified:Boolean(loginFinish.sessionKey),timingsMs:{registrationStart:regRequestDone-regStartAt,registrationServerExchange:regResponseAt-regRequestDone,registrationFinish:registrationDone-regResponseAt,loginRoundTrip:loginDone-loginStartAt,loginClientStart:clientLoginStartDone-clientLoginStart,loginServerExchange:loginResponseDone-clientLoginStartDone,loginClientFinish:clientLoginFinishDone-loginResponseDone}, jsHeapBytes:performance.memory?.usedJSHeapSize ?? null};
    return out;
  });
  if (outcome.diagnostic) throw new Error(JSON.stringify(outcome.diagnostic));
  assert.equal(outcome.loginFinishStatus.status, 204, JSON.stringify(outcome));
  assert.equal(outcome.clientKeyProducedAndKe3Verified, true, JSON.stringify(outcome));
  const traffic = requests.join('\n');
  for (const forbidden of ['correct horse battery staple', 'clientLoginState', 'clientRegistrationState', 'sessionKey', 'exportKey']) assert.equal(traffic.includes(forbidden), false, `secret leaked in HTTP body: ${forbidden}`);
  const rss = chromiumRss();
  outcome.initializationMs = await page.evaluate(() => window.opaqueInitMs);
  console.log(JSON.stringify({outcome, requestCount:requests.length, chromium: await browser.version(), chromiumHeadlessProcesses:{count:rss.length,baselineRssKiB:baselineRss.reduce((a,b)=>a+b,0),afterProtocolRssKiB:rss.reduce((a,b)=>a+b,0)}, serverLog}, null, 2));
} catch (error) {
  console.error(serverLog); throw error;
} finally {
  if (browser) await browser.close();
  server.kill('SIGTERM');
  await new Promise(resolve => server.once('exit', resolve));
}
