import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { copyFile, mkdtemp, rm, chmod } from "node:fs/promises";
import { chromium } from "playwright";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const root = new URL("../../", import.meta.url).pathname;
const dir = await mkdtemp(join(tmpdir(), "deploy-manager-browser-"));
const executable = join(dir, "deploy-manager");
let server;
let browser;
let serverOutput = "";
const testPassword = "Correct horse battery staple 2026!";

function redact(value, grant = "") {
  let text = String(value);
  if (grant) text = text.replaceAll(grant, "<REDACTED>");
  return text
    .replaceAll(testPassword, "<REDACTED>")
    .replace(/#[^\s"'<>]*/g, "#<REDACTED>");
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
  if (nestedRoute.status !== 200 || !(await nestedRoute.text()).includes("_app/immutable/entry/start.")) throw new Error("nested client route did not return the SPA shell");

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
  await context.close();
  console.log("combined-binary browser regression passed: embedded SPA routes/assets, lazy OPAQUE, protected registration, one-time grants, and restart durability");
} finally {
  await browser?.close();
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    await Promise.race([once(server, "exit"), new Promise(resolve => setTimeout(resolve, 1000))]);
  }
  await rm(dir, { recursive: true, force: true });
}
