import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { copyFile, mkdtemp, rm, chmod } from "node:fs/promises";
import { chromium } from "playwright";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { request as httpsRequest } from "node:https";

const root = new URL("../../", import.meta.url).pathname;
const dir = await mkdtemp(join(tmpdir(), "deploy-manager-browser-"));
const executable = join(dir, "deploy-manager");
let server;
let browser;
let serverOutput = "";
let testPassword = "Correct horse battery staple 2026!";

function redact(value, grant = "") {
  let text = String(value);
  if (grant) text = text.replaceAll(grant, "<REDACTED>");
  return text
    .replaceAll(testPassword, "<REDACTED>")
    .replace(/#[^\s"'<>]*/g, "#<REDACTED>");
}

function httpsStatus(hostname, port, path, headers = {}) {
  return new Promise((resolve, reject) => {
    const req = httpsRequest({ hostname, port, path, method: "GET", rejectUnauthorized: false, headers }, response => {
      response.resume();
      response.on("end", () => resolve(response.statusCode));
    });
    req.on("error", reject);
    req.end();
  });
}

function httpsPostStatus(hostname, port, path, origin, body) {
  return new Promise((resolve, reject) => {
    const payload = JSON.stringify(body);
    const req = httpsRequest({ hostname, port, path, method: "POST", rejectUnauthorized: false, headers: { Origin: origin, "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) } }, response => {
      response.resume();
      response.on("end", () => resolve(response.statusCode));
    });
    req.on("error", reject);
    req.end(payload);
  });
}

async function navigate(page, url, options = {}) {
  try {
    return await page.goto(url, options);
  } catch {
    throw new Error("Browser navigation failed (URL and fragment redacted)");
  }
}

try {
  let builtBinary = process.env.DEPLOY_MANAGER_BINARY;
  if (!builtBinary) {
    const build = spawnSync("bash", ["scripts/build.sh"], { cwd: root, encoding: "utf8" });
    if (build.status !== 0) throw new Error(build.stderr || "Combined build failed");
    builtBinary = join(root, "deploy-manager");
  }
  await copyFile(resolve(builtBinary), executable);
  await chmod(executable, 0o700);

  const listen = "127.0.0.1:18437";
  const setup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { cwd: dir, encoding: "utf8" });
  if (setup.status !== 0) throw new Error(setup.stderr || "CLI setup failed");
  const link = setup.stdout.trim();
  const issuedURL = new URL(link);
  const grant = issuedURL.hash.slice(1);
  if (issuedURL.origin !== `http://${listen}` || issuedURL.pathname !== "/register" || issuedURL.search || !grant) {
    throw new Error("setup CLI did not issue a fragment-only loopback setup URL");
  }
  const failedInitSetup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { cwd: dir, encoding: "utf8" });
  if (failedInitSetup.status !== 0) throw new Error(failedInitSetup.stderr || "second setup grant failed");
  const failedInitLink = failedInitSetup.stdout.trim();

  const startServer = async () => {
    server = spawn(executable, ["serve", "--data", dir, "--listen", listen], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
    server.stdout.on("data", chunk => { serverOutput += chunk.toString(); });
    server.stderr.on("data", chunk => { serverOutput += chunk.toString(); });
    await new Promise((resolve, reject) => {
      const started = Date.now();
      const poll = async () => {
        try {
          const response = await fetch(`http://${listen}/register`, { headers: { Accept: "text/html" } });
          if (response.status === 200) return resolve();
        } catch {}
        if (Date.now() - started > 10000) return reject(new Error(`application server did not start: ${serverOutput}`));
        setTimeout(poll, 100);
      };
      poll();
    });
  };
  await startServer();

  const pageResponse = await fetch(`http://${listen}/register`, { headers: { Accept: "text/html" } });
  const shell = await pageResponse.text();
  if (pageResponse.status !== 200 || !shell.includes("_app/immutable/entry/start.")) throw new Error("registration route did not return the embedded SPA shell");
  if (pageResponse.headers.get("cache-control") !== "no-cache" || !pageResponse.headers.get("content-security-policy")?.includes("sha256-")) {
    throw new Error("SPA shell caching or CSP headers are incorrect");
  }
  if (shell.includes(grant) || shell.includes("opaque-server.key")) throw new Error("SPA shell included protected setup material");

  const missingAsset = await fetch(`http://${listen}/_app/immutable/entry/missing.deadbeef.js`, { headers: { Accept: "text/html" } });
  if (missingAsset.status !== 404 || missingAsset.headers.get("content-type")?.includes("text/html")) throw new Error("missing assets must remain non-HTML 404 responses");
  const wrongMethod = await fetch(`http://${listen}/register`, { method: "POST", headers: { Accept: "text/html" } });
  if (wrongMethod.status !== 405) throw new Error(`unsupported frontend method returned ${wrongMethod.status}, expected 405`);
  const wrongProtocolMethod = await fetch(`http://${listen}/opaque/register/start`);
  if (wrongProtocolMethod.status !== 405) throw new Error(`protocol path returned SPA or wrong status for GET: ${wrongProtocolMethod.status}`);
  const nestedRoute = await fetch(`http://${listen}/dashboard/projects/nested`, { headers: { Accept: "text/html" } });
  if (nestedRoute.status !== 426) throw new Error(`dashboard route over plain HTTP returned ${nestedRoute.status}, expected 426`);
  const plainLogin = await fetch(`http://${listen}/login`, { headers: { Accept: "text/html" } });
  if (plainLogin.status !== 426) throw new Error(`login page over plain HTTP returned ${plainLogin.status}, expected 426`);

  const browserPath = process.env.CHROMIUM_PATH || chromium.executablePath();
  browser = await chromium.launch({ headless: true, executablePath: browserPath, args: ["--no-sandbox"] });
  const context = await browser.newContext();
  const observedURLs = [];
  const largeJavaScript = [];
  context.on("request", request => observedURLs.push(request.url()));
  context.on("response", response => {
    if (response.headers()["content-type"]?.includes("javascript") && Number(response.headers()["content-length"]) > 300000) {
      largeJavaScript.push(response.url());
    }
  });

  const failedInitPage = await context.newPage();
  await failedInitPage.addInitScript(() => Object.defineProperty(globalThis, "WebAssembly", { value: undefined }));
  let initRequests = 0;
  const initPageErrors = [];
  const initFailedScripts = [];
  failedInitPage.on("pageerror", error => initPageErrors.push(redact(`${error.name}: ${error.message}`, new URL(failedInitLink).hash.slice(1))));
  failedInitPage.on("console", message => {
    if (message.type() === "error") initPageErrors.push(redact(message.text(), new URL(failedInitLink).hash.slice(1)));
  });
  failedInitPage.on("requestfailed", request => {
    if (request.resourceType() === "script") {
      const url = new URL(request.url());
      initFailedScripts.push(`${url.pathname}: ${redact(request.failure()?.errorText || "request failed")}`);
    }
  });
  failedInitPage.on("request", request => { if (request.method() === "POST") initRequests++; });
  await failedInitPage.addInitScript(() => {
    globalThis.__cspViolations = [];
    document.addEventListener("securitypolicyviolation", event => {
      // The expected WebAssembly initialization error is tested separately; CSP violations are never expected.
      globalThis.__cspViolations.push(`${event.effectiveDirective}: ${event.blockedURI}`);
    });
  });
  await navigate(failedInitPage, failedInitLink);
  await failedInitPage.waitForTimeout(100);
  if (await failedInitPage.evaluate(() => globalThis.__cspViolations.length)) throw new Error(`production CSP blocked page styling or resources during the WASM-init failure case: ${await failedInitPage.evaluate(() => globalThis.__cspViolations.join(", "))}`);
  try {
    await failedInitPage.getByLabel("Password").waitFor({ state: "visible", timeout: 10000 });
  } catch {
    const visibleURL = new URL(failedInitPage.url());
    throw new Error(`app-ready failed after page load: ${JSON.stringify({
      pathname: visibleURL.pathname,
      hashPresent: Boolean(visibleURL.hash),
      pageErrors: initPageErrors.slice(0, 5),
      failedScripts: initFailedScripts.slice(0, 5)
    })}`);
  }
  await failedInitPage.getByLabel("Password").fill(testPassword);
  const initChunk = failedInitPage.waitForResponse(response => response.headers()["content-type"]?.includes("javascript") && Number(response.headers()["content-length"]) > 300000);
  await failedInitPage.getByRole("button", { name: "Create account" }).click();
  await initChunk;
  await failedInitPage.getByText(/Check that JavaScript and WebAssembly are enabled/).waitFor({ timeout: 30000 });
  if (initRequests !== 0) throw new Error("initialization failure attempted an enrollment request");
  if (await failedInitPage.evaluate(() => globalThis.__cspViolations.length)) throw new Error(`CSP violation occurred during the deliberate WASM-init failure case: ${await failedInitPage.evaluate(() => globalThis.__cspViolations.join(", "))}`);
  await failedInitPage.close();

  const noGrantPage = await context.newPage();
  let noGrantPosts = 0;
  noGrantPage.on("request", request => { if (request.method() === "POST") noGrantPosts++; });
  await noGrantPage.goto(`http://${listen}/register`);
  await noGrantPage.getByText(/setup link is incomplete/).waitFor({ timeout: 30000 });
  if (noGrantPosts !== 0) throw new Error("missing grant fell back to an unprotected registration request");
  await noGrantPage.close();

  const page = await context.newPage();
  const postedBodies = [];
  const pageErrors = [];
  const failedScripts = [];
  const failedRequests = [];
  page.on("pageerror", error => pageErrors.push(redact(`${error.name}: ${error.message}`, grant)));
  page.on("console", message => {
    if (message.type() === "error") pageErrors.push(redact(message.text(), grant));
  });
  await page.addInitScript(() => {
    globalThis.__cspViolations = [];
    document.addEventListener("securitypolicyviolation", event => {
      globalThis.__cspViolations.push(`${event.effectiveDirective}: ${event.blockedURI}`);
    });
  });
  page.on("requestfailed", request => {
    const url = new URL(request.url());
    failedRequests.push(`${request.resourceType()} ${url.pathname}: ${redact(request.failure()?.errorText || "request failed", grant)}`);
    if (request.resourceType() === "script") {
      failedScripts.push(`${url.pathname}: ${redact(request.failure()?.errorText || "request failed", grant)}`);
    }
  });
  page.on("request", request => { if (request.method() === "POST") postedBodies.push(request.postData() || ""); });
  let finishRaceStatuses;
  await page.route("**/opaque/register/finish", async route => {
    const results = await Promise.all([route.fetch(), route.fetch()]);
    finishRaceStatuses = results.map(result => result.status()).sort((a, b) => a - b);
    const accepted = results.find(result => result.status() === 204);
    if (!accepted) throw new Error(`registration race had no accepted request: ${finishRaceStatuses}`);
    await route.fulfill({ response: accepted });
  });
  const response = await navigate(page, link, { waitUntil: "domcontentloaded" });
  const hashAtDOMContentLoaded = Boolean(new URL(page.url()).hash);
  await page.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  await page.waitForTimeout(100);
  if (await page.evaluate(() => globalThis.__cspViolations.length)) throw new Error(`production CSP blocked page styling or resources: ${await page.evaluate(() => globalThis.__cspViolations.join(", "))}`);
  const hashWhenFormReady = Boolean(new URL(page.url()).hash);
  try {
    await page.waitForFunction(() => window.location.hash === "", null, { timeout: 1500 });
  } catch {}
  const hashAfterRetry = Boolean(new URL(page.url()).hash);
  if (hashWhenFormReady || hashAfterRetry || !page.url().endsWith("/register")) {
    const signals = JSON.stringify({
      navigationStatus: response?.status() ?? null,
      hashPresentAtDOMContentLoaded: hashAtDOMContentLoaded,
      passwordFormVisible: true,
      hashPresentWhenFormReady: hashWhenFormReady,
      hashPresentAfterRetry: hashAfterRetry,
      pageErrors: pageErrors.slice(0, 5),
      failedScripts: failedScripts.slice(0, 5),
      failedRequests: failedRequests.slice(0, 5)
    });
    throw new Error(`grant-fragment cleanup failed at app-ready check: ${signals}`);
  }
  await page.getByRole("link", { name: "Return to start" }).click();
  await page.getByRole("link", { name: "Sign in" }).waitFor({ state: "visible", timeout: 30000 });
  await page.getByRole("link", { name: "Open administrator setup" }).click();
  await page.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  if (new URL(page.url()).hash) throw new Error("client navigation restored the setup grant in the URL");
  if (largeJavaScript.length !== 1) throw new Error("OPAQUE WASM bundle was loaded before registration began");
  const largeBundlesBeforeSubmit = largeJavaScript.length;
  await page.getByLabel("Password").fill(testPassword);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByText("Administrator account created. You can close this page.").waitFor({ timeout: 60000 });
  if (largeJavaScript.length <= largeBundlesBeforeSubmit) throw new Error("OPAQUE bundle was not loaded when registration started");
  if (JSON.stringify(finishRaceStatuses) !== JSON.stringify([204, 410])) throw new Error(`grant consumption race was not single-use: ${finishRaceStatuses}`);
  if (observedURLs.some(url => url.includes(grant))) throw new Error("registration grant leaked into a browser request URL");
  if (!postedBodies.length || postedBodies.some(body => !body.includes(grant))) throw new Error("grant was not sent with protected registration POST bodies");
  if (await context.cookies().then(cookies => cookies.length)) throw new Error("registration unexpectedly issued a session cookie");
  for (const body of postedBodies) {
    if (body.includes("Correct horse battery staple") || body.includes("clientRegistrationState") || body.includes("exportKey") || body.includes("sessionKey")) {
      throw new Error("browser transmitted a password or local OPAQUE secret state");
    }
  }
  const consumedGrant = await fetch(`http://${listen}/opaque/register/start`, {
    method: "POST",
    headers: { Origin: `http://${listen}`, "Content-Type": "application/json" },
    body: JSON.stringify({ grant, value: "AA" })
  });
  if (consumedGrant.status !== 410) throw new Error(`consumed grant was not disabled: ${consumedGrant.status}`);
  const signupClosed = await fetch(`http://${listen}/opaque/register/start`, {
    method: "POST",
    headers: { Origin: `http://${listen}`, "Content-Type": "application/json" },
    body: JSON.stringify({ grant: new URL(failedInitLink).hash.slice(1), value: "AA" })
  });
  if (signupClosed.status !== 410) throw new Error(`registration remained open after account creation: ${signupClosed.status}`);
  const issuedTokens = [grant, new URL(failedInitLink).hash.slice(1)];
  if (issuedTokens.some(token => serverOutput.includes(token))) throw new Error("setup grants appeared in routine application logs");

  server.kill("SIGTERM");
  await once(server, "exit");
  await startServer();
  const secondSetup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { cwd: dir, encoding: "utf8" });
  if (secondSetup.status === 0) throw new Error("CLI issued a new grant after administrator registration");
  const restartGrant = await fetch(`http://${listen}/opaque/register/start`, {
    method: "POST",
    headers: { Origin: `http://${listen}`, "Content-Type": "application/json" },
    body: JSON.stringify({ grant, value: "AA" })
  });
  if (restartGrant.status !== 410) throw new Error(`administrator state was not durable across restart: ${restartGrant.status}`);

  // Login and session behavior is exercised over real TLS so Secure cookies are never weakened for tests.
  server.kill("SIGTERM");
  await once(server, "exit");
  const secureListen = "127.0.0.1:18438";
  const cert = join(dir, "localhost.crt");
  const key = join(dir, "localhost.key");
  const secureOrigin = `https://127.0.0.1:${secureListen.split(":")[1]}`;
  const certificate = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert, "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1", "-days", "1"], { encoding: "utf8" });
  if (certificate.status !== 0) throw new Error(certificate.stderr || "Could not create test TLS certificate");
  server = spawn(executable, ["serve", "--data", dir, "--listen", secureListen, "--tls-cert", cert, "--tls-key", key, "--origin", secureOrigin, "--login-ttl", "3s", "--session-ttl", "30s", "--recent-auth-ttl", "1s", "--auth-rate-per-minute", "100"], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  server.stdout.on("data", chunk => { serverOutput += chunk.toString(); });
  server.stderr.on("data", chunk => { serverOutput += chunk.toString(); });
  await new Promise((resolve, reject) => {
    const started = Date.now();
    const poll = async () => {
      try {
        if (await httpsStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/login", { Accept: "text/html" }) === 200) return resolve();
      } catch {}
      if (Date.now() - started > 10000) return reject(new Error(`HTTPS application did not start: ${serverOutput}`));
      setTimeout(poll, 100);
    };
    poll();
  });
  const wrongHost = await httpsStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/login", { Accept: "text/html", Host: "attacker.example" });
  if (wrongHost !== 421) throw new Error(`non-canonical Host header returned ${wrongHost}, expected 421`);
  const crossOriginLogin = await httpsPostStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/opaque/login/start", "https://attacker.example", { value: "AA" });
  if (crossOriginLogin !== 403) throw new Error(`cross-origin login start returned ${crossOriginLogin}, expected 403`);

  const authContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const authPage = await authContext.newPage();
  const authBodies = [];
  const authCryptoAssets = [];
  await authPage.addInitScript(() => {
    globalThis.__cspViolations = [];
    document.addEventListener("securitypolicyviolation", event => globalThis.__cspViolations.push(`${event.effectiveDirective}: ${event.blockedURI}`));
  });
  authPage.on("request", request => {
    if (request.method() === "POST") authBodies.push(request.postData() || "");
  });
  authPage.on("response", response => {
    if (response.headers()["content-type"]?.includes("javascript") && Number(response.headers()["content-length"]) > 300000) authCryptoAssets.push(response.url());
  });
  await authPage.goto(`${secureOrigin}/dashboard/projects/nested`, { waitUntil: "domcontentloaded" });
  await authPage.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  if (await authPage.evaluate(() => globalThis.__cspViolations.length)) throw new Error(`CSP blocked login resources: ${await authPage.evaluate(() => globalThis.__cspViolations.join(", "))}`);
  const unauthStatus = await authPage.evaluate(async () => (await fetch("/api/session")).status);
  if (unauthStatus !== 401) throw new Error(`direct unauthenticated session read returned ${unauthStatus}`);
  if (authCryptoAssets.length) throw new Error("dashboard/login loaded OPAQUE before an authentication attempt");

  await authPage.getByLabel("Password").fill("Wrong password 2026!");
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.getByText("Sign-in failed. Check your password and try again.").waitFor({ timeout: 30000 });
  if (authCryptoAssets.length !== 1) throw new Error(`login did not load exactly one lazy OPAQUE asset: ${authCryptoAssets.length}`);
  if ((await authContext.cookies()).length) throw new Error("incorrect password created a session cookie");

  let expiredHandshakeStatus;
  const expiredHandshake = new Promise(resolve => { expiredHandshakeStatus = resolve; });
  await authPage.route("**/opaque/login/finish", async route => {
    await new Promise(resolve => setTimeout(resolve, 3200));
    const response = await route.fetch();
    expiredHandshakeStatus(response.status());
    await route.fulfill({ response });
  });
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  const expiredStatus = await expiredHandshake;
  await authPage.waitForFunction(() => document.querySelector('[role="status"]')?.textContent?.includes("Sign-in failed"), null, { timeout: 30000 });
  if (expiredStatus !== 401 || (await authContext.cookies()).length) throw new Error(`expired login handshake returned ${expiredStatus} or issued a cookie`);
  await authPage.unroute("**/opaque/login/finish");

  let tamperedResponse;
  let finishTampered;
  const tamperedCompletion = new Promise(resolve => { finishTampered = resolve; });
  await authPage.route("**/opaque/login/finish", async route => {
    const body = JSON.parse(route.request().postData());
    const bytes = Buffer.from(body.value.replaceAll("-", "+").replaceAll("_", "/"), "base64");
    bytes[0] ^= 1;
    body.value = bytes.toString("base64url");
    tamperedResponse = await route.fetch({ postData: JSON.stringify(body) });
    await route.fulfill({ response: tamperedResponse });
    finishTampered(tamperedResponse.status());
  });
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.waitForFunction(() => document.querySelector('[role="status"]')?.textContent?.includes("Sign-in failed"), null, { timeout: 30000 });
  const tamperedStatus = await tamperedCompletion;
  if (tamperedStatus !== 401 || (await authContext.cookies()).length) throw new Error(`tampered KE3 authenticated or issued a cookie (status ${tamperedStatus}, cookies ${(await authContext.cookies()).length})`);
  await authPage.unroute("**/opaque/login/finish");

  let finishBody;
  await authPage.route("**/opaque/login/finish", async route => {
    finishBody = route.request().postData();
    await route.continue();
  });
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  await authPage.waitForTimeout(1200);
  const staleAuthChange = await authPage.evaluate(async () => (await fetch("/opaque/password/change/start", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ value: "AA" }) })).status);
  if (staleAuthChange !== 401) throw new Error(`password change accepted stale OPAQUE authentication (${staleAuthChange})`);
  if (await authPage.evaluate(() => globalThis.__cspViolations.length)) throw new Error(`CSP blocked dashboard resources: ${await authPage.evaluate(() => globalThis.__cspViolations.join(", "))}`);
  const sessionCookie = (await authContext.cookies()).find(cookie => cookie.name === "__Host-deploy-manager-session");
  if (!sessionCookie || !sessionCookie.secure || !sessionCookie.httpOnly || sessionCookie.sameSite !== "Strict") throw new Error("session cookie attributes are not Secure, HttpOnly, and SameSite=Strict");
  if (!finishBody || finishBody.includes(testPassword) || finishBody.includes("clientLoginState") || finishBody.includes("sessionKey")) throw new Error("login transmitted password or local OPAQUE secret state");
  const authenticated = await authPage.evaluate(async () => (await fetch("/api/session")).status);
  if (authenticated !== 200) throw new Error(`authenticated session check returned ${authenticated}`);
  await authPage.goto(`${secureOrigin}/dashboard/projects/nested`, { waitUntil: "domcontentloaded" });
  await authPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  await authPage.reload({ waitUntil: "domcontentloaded" });
  await authPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  await authPage.waitForFunction(async () => (await fetch("/api/session")).status === 401, null, { timeout: 35000 });
  const expiredLogout = await authPage.evaluate(async () => (await fetch("/api/logout", { method: "POST" })).status);
  if (expiredLogout !== 204) throw new Error(`logout after session expiry returned ${expiredLogout}`);
  await authPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  try {
    await authPage.locator("#password").waitFor({ state: "visible", timeout: 10000 });
  } catch {
    const state = await authPage.evaluate(() => ({ url: location.href, title: document.title, text: document.body.innerText.slice(0, 500) }));
    throw new Error(`login page did not appear after session expiry: ${JSON.stringify(state)}`);
  }
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  const finish = JSON.parse(finishBody);
  const replay = await authPage.evaluate(async payload => (await fetch("/opaque/login/finish", { method: "POST", headers: { "Content-Type": "application/json" }, body: payload })).status, JSON.stringify(finish));
  if (replay !== 401) throw new Error(`replayed KE3 returned ${replay}, expected 401`);
  const crossOriginStatus = await new Promise((resolve, reject) => {
    const req = httpsRequest({ hostname: "127.0.0.1", port: Number(secureListen.split(":")[1]), path: "/api/logout", method: "POST", rejectUnauthorized: false, headers: { Origin: "https://attacker.example", Cookie: `${sessionCookie.name}=${sessionCookie.value}` } }, response => { response.resume(); response.on("end", () => resolve(response.statusCode)); });
    req.on("error", reject);
    req.end();
  });
  if (crossOriginStatus !== 403) throw new Error(`cross-origin logout returned ${crossOriginStatus}, expected 403`);
  await authPage.getByRole("button", { name: "Sign out" }).click();
  await authPage.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  if (await authPage.evaluate(async () => (await fetch("/api/session")).status) !== 401) throw new Error("logout did not revoke the server session");

  const unauthenticatedChange = await authPage.evaluate(async () => (await fetch("/opaque/password/change/start", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ value: "AA" }) })).status);
  if (unauthenticatedChange !== 401) throw new Error(`password change without recent OPAQUE auth returned ${unauthenticatedChange}`);
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.getByText(/signed in/).waitFor({ timeout: 30000 });

  const inflightContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const inflightPage = await inflightContext.newPage();
  await inflightPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  let staleLogin;
  await inflightPage.route("**/opaque/login/finish", async route => {
    staleLogin = JSON.parse(route.request().postData());
    await route.fulfill({ status: 503, body: "held for credential replacement" });
  });
  await inflightPage.getByLabel("Password").fill(testPassword);
  await inflightPage.getByRole("button", { name: "Sign in" }).click();
  await inflightPage.getByText("Sign-in failed. Check your password and try again.").waitFor({ timeout: 30000 });
  if (!staleLogin?.transaction) throw new Error("could not create a pre-replacement login handshake");
  const secondSessionContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const secondSessionPage = await secondSessionContext.newPage();
  await secondSessionPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  await secondSessionPage.getByLabel("Password").fill(testPassword);
  await secondSessionPage.getByRole("button", { name: "Sign in" }).click();
  await secondSessionPage.getByText(/signed in/).waitFor({ timeout: 30000 });

  const passwordChangeBodies = [];
  const passwordLoginResults = [];
  authPage.on("request", request => { if (request.method() === "POST") passwordChangeBodies.push(request.postData() || ""); });
  authPage.on("response", response => { if (response.url().includes("/opaque/login/")) passwordLoginResults.push(`${new URL(response.url()).pathname}:${response.status()}`); });
  await authPage.goto(`${secureOrigin}/account/password`, { waitUntil: "domcontentloaded" });
  await authPage.getByLabel("Current password").fill(testPassword);
  const changedPassword = "A new and very secure password 2026!";
  await authPage.getByLabel("New password").fill(changedPassword);
  await authPage.getByRole("button", { name: "Change password" }).click();
  await authPage.getByText(/Password changed/).waitFor({ timeout: 60000 });
  if (await authPage.evaluate(async () => (await fetch("/api/session")).status) !== 401) throw new Error("credential replacement did not revoke the current session");
  if (await secondSessionPage.evaluate(async () => (await fetch("/api/session")).status) !== 401) throw new Error("credential replacement did not revoke another active session");
  if (passwordChangeBodies.some(body => body.includes(testPassword) || body.includes(changedPassword) || body.includes("clientLoginState") || body.includes("clientRegistrationState") || body.includes("sessionKey"))) throw new Error("password change sent a password or local OPAQUE secret state");
  const staleStatus = await authPage.evaluate(async payload => (await fetch("/opaque/login/finish", { method: "POST", headers: { "Content-Type": "application/json" }, body: payload })).status, JSON.stringify(staleLogin));
  if (staleStatus !== 401) throw new Error(`pre-replacement login handshake issued a stale session (${staleStatus})`);
  await inflightContext.close();
  await secondSessionContext.close();
  testPassword = changedPassword;
  await authPage.getByRole("button", { name: "Continue to sign in" }).click();
  await authPage.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  await authPage.getByLabel("Password").fill("Correct horse battery staple 2026!");
  await authPage.getByRole("button", { name: "Sign in" }).click();
  await authPage.getByText("Sign-in failed. Check your password and try again.").waitFor({ timeout: 30000 });
  await authPage.getByLabel("Password").fill(testPassword);
  await authPage.getByRole("button", { name: "Sign in" }).click();
  try { await authPage.getByText(/signed in/).waitFor({ timeout: 30000 }); }
  catch { throw new Error(`new password login failed: ${await authPage.locator('[role="status"]').allTextContents()} (${passwordLoginResults.join(",")})`); }
  const oldCookie = (await authContext.cookies()).find(cookie => cookie.name === "__Host-deploy-manager-session");
  if (!oldCookie) throw new Error("could not capture active session before restart");

  const pendingContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const pendingPage = await pendingContext.newPage();
  await pendingPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  await pendingPage.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  let interruptedLogin;
  await pendingPage.route("**/opaque/login/finish", async route => {
    interruptedLogin = JSON.parse(route.request().postData());
    await route.fulfill({ status: 503, body: "interrupted" });
  });
  await pendingPage.getByLabel("Password").fill(testPassword);
  await pendingPage.getByRole("button", { name: "Sign in" }).click();
  await pendingPage.getByText("Sign-in failed. Check your password and try again.").waitFor({ timeout: 30000 });
  if (!interruptedLogin?.transaction) throw new Error("could not create the pending login used for restart coverage");

  server.kill("SIGTERM");
  await once(server, "exit");
  server = spawn(executable, ["serve", "--data", dir, "--listen", secureListen, "--tls-cert", cert, "--tls-key", key, "--origin", secureOrigin, "--recent-auth-ttl", "1s", "--auth-rate-per-minute", "100"], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  server.stdout.on("data", chunk => { serverOutput += chunk.toString(); });
  server.stderr.on("data", chunk => { serverOutput += chunk.toString(); });
  await new Promise((resolve, reject) => {
    const started = Date.now();
    const poll = async () => {
      try {
        if (await httpsStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/api/session", { Cookie: `${oldCookie.name}=${oldCookie.value}` }) === 401) return resolve();
      } catch {}
      if (Date.now() - started > 10000) return reject(new Error(`HTTPS application did not restart: ${serverOutput}`));
      setTimeout(poll, 100);
    };
    poll();
  });
  const interruptedStatus = await httpsPostStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/opaque/login/finish", secureOrigin, interruptedLogin);
  if (interruptedStatus !== 401) throw new Error(`pre-restart login handshake survived restart: ${interruptedStatus}`);
  await pendingContext.close();

  const expiredRecoveryCommand = spawnSync(executable, ["recover", "--data", dir, "--listen", secureListen, "--url", secureOrigin, "--grant-ttl", "1s"], { cwd: dir, encoding: "utf8" });
  if (expiredRecoveryCommand.status !== 0) throw new Error(expiredRecoveryCommand.stderr || "short recovery CLI failed");
  const expiredGrant = new URL(expiredRecoveryCommand.stdout.trim()).hash.slice(1);
  await new Promise(resolve => setTimeout(resolve, 1100));
  const expiredRecoveryStatus = await httpsPostStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/opaque/register/start", secureOrigin, { grant: expiredGrant, value: "AA" });
  if (expiredRecoveryStatus !== 410) throw new Error(`expired recovery grant returned ${expiredRecoveryStatus}, expected 410`);
  const recoveryCommand = spawnSync(executable, ["recover", "--data", dir, "--listen", secureListen, "--url", secureOrigin], { cwd: dir, encoding: "utf8" });
  if (recoveryCommand.status !== 0) throw new Error(recoveryCommand.stderr || "recovery CLI failed");
  const recoveryLink = recoveryCommand.stdout.trim();
  const recoveryURL = new URL(recoveryLink);
  const recoveryGrant = recoveryURL.hash.slice(1);
  if (recoveryURL.pathname !== "/recover" || recoveryURL.origin !== secureOrigin || !recoveryGrant || recoveryURL.search) throw new Error("recovery CLI did not issue a fragment-only recovery URL");
  const recoveryContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const recoveryPage = await recoveryContext.newPage();
  const recoveryBodies = [];
  const recoveryURLs = [];
  recoveryPage.on("request", request => { recoveryURLs.push(request.url()); if (request.method() === "POST") recoveryBodies.push(request.postData() || ""); });
  await recoveryPage.goto(recoveryLink, { waitUntil: "domcontentloaded" });
  await recoveryPage.getByLabel("Replace password").waitFor({ state: "visible", timeout: 30000 });
  await recoveryPage.waitForFunction(() => location.hash === "", null, { timeout: 10000 });
  if (recoveryPage.url().includes(recoveryGrant)) throw new Error("recovery grant remained in the visible URL");
  const recoveredPassword = "Recovered with a stronger password 2026!";
  await recoveryPage.getByLabel("Replace password").fill(recoveredPassword);
  const injectedFailure = spawnSync("go", ["run", join(root, "tests/fault/sqlite-trigger.go"), join(dir, "state.sqlite"), "install"], { cwd: root, encoding: "utf8" });
  if (injectedFailure.status !== 0) throw new Error(injectedFailure.stderr || "could not install replacement fault injection");
  await recoveryPage.getByRole("button", { name: "Replace password" }).click();
  await recoveryPage.getByText("Recovery could not be completed. Check your connection and try again.").waitFor({ timeout: 30000 });
  const preservedContext = await browser.newContext({ ignoreHTTPSErrors: true });
  const preservedPage = await preservedContext.newPage();
  await preservedPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  await preservedPage.getByLabel("Password").fill(testPassword);
  await preservedPage.getByRole("button", { name: "Sign in" }).click();
  await preservedPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  await preservedContext.close();
  const removeFailure = spawnSync("go", ["run", join(root, "tests/fault/sqlite-trigger.go"), join(dir, "state.sqlite"), "remove"], { cwd: root, encoding: "utf8" });
  if (removeFailure.status !== 0) throw new Error(removeFailure.stderr || "could not remove replacement fault injection");
  let recoveryRaceStatuses;
  await recoveryPage.route("**/opaque/register/finish", async route => {
    const results = await Promise.all([route.fetch(), route.fetch()]);
    recoveryRaceStatuses = results.map(result => result.status()).sort((a, b) => a - b);
    const accepted = results.find(result => result.status() === 204);
    if (!accepted) throw new Error(`recovery race had no accepted request: ${recoveryRaceStatuses}`);
    await route.fulfill({ response: accepted });
  });
  await recoveryPage.getByLabel("Replace password").fill(recoveredPassword);
  await recoveryPage.getByRole("button", { name: "Replace password" }).click();
  await recoveryPage.getByText("Administrator password replaced. You can close this page.").waitFor({ timeout: 60000 });
  if (JSON.stringify(recoveryRaceStatuses) !== JSON.stringify([204, 410])) throw new Error(`recovery grant was not atomically consumed: ${recoveryRaceStatuses}`);
  if (recoveryURLs.some(url => url.includes(recoveryGrant))) throw new Error("recovery grant leaked into a request URL");
  if (!recoveryBodies.length || recoveryBodies.some(body => !body.includes(recoveryGrant) || body.includes(recoveredPassword) || body.includes("clientRegistrationState"))) throw new Error("recovery requests omitted grant or transmitted a password/client state");
  const reusedRecovery = await httpsPostStatus("127.0.0.1", Number(secureListen.split(":")[1]), "/opaque/register/start", secureOrigin, { grant: recoveryGrant, value: "AA" });
  if (reusedRecovery !== 410) throw new Error(`spent recovery grant returned ${reusedRecovery}, expected 410`);
  await recoveryPage.goto(`${secureOrigin}/login`, { waitUntil: "domcontentloaded" });
  await recoveryPage.getByLabel("Password").fill(testPassword);
  await recoveryPage.getByRole("button", { name: "Sign in" }).click();
  await recoveryPage.getByText("Sign-in failed. Check your password and try again.").waitFor({ timeout: 30000 });
  await recoveryPage.getByLabel("Password").fill(recoveredPassword);
  await recoveryPage.getByRole("button", { name: "Sign in" }).click();
  await recoveryPage.getByText(/signed in/).waitFor({ timeout: 30000 });
  await recoveryContext.close();
  if (serverOutput.includes(recoveryGrant)) throw new Error("recovery grant appeared in routine application logs");
  if (authBodies.some(body => body.includes(testPassword) || body.includes("clientLoginState") || body.includes("sessionKey"))) throw new Error("browser login traffic contained password or OPAQUE secret state");
  await authContext.close();
  await context.close();
  console.log("combined-binary browser regression passed: registration, login, secure password change, recovery, atomic credential replacement, session/handshake revocation, and restart invalidation");
} finally {
  await browser?.close();
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    await Promise.race([once(server, "exit"), new Promise(resolve => setTimeout(resolve, 1000))]);
  }
  await rm(dir, { recursive: true, force: true });
}
