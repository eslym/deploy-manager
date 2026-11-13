import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { chmod, copyFile, mkdtemp, rm } from "node:fs/promises";
import { request as httpsRequest } from "node:https";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { chromium } from "playwright";

const root = new URL("../../", import.meta.url).pathname;
const dir = await mkdtemp(join(tmpdir(), "deploy-manager-project-browser-"));
const executable = join(dir, "deploy-manager");
const appListen = "127.0.0.1:18440";
const fixtureListen = "127.0.0.1:18441";
const fixtureOrigin = `https://${fixtureListen}`;
const fixtureURL = `${fixtureOrigin}/repo.git`;
const password = "Project browser test password 2026!";
let app;
let fixture;
let browser;
let appOutput = "";

function httpsRequestStatus(host, port, path, method = "GET", body = "", headers = {}) {
  return new Promise((resolveStatus, reject) => {
    const request = httpsRequest({ hostname: host, port, path, method, rejectUnauthorized: false, headers }, response => {
      response.resume();
      response.on("end", () => resolveStatus(response.statusCode));
    });
    request.on("error", reject);
    request.end(body);
  });
}

async function waitFor(check, label) {
  const started = Date.now();
  while (Date.now() - started < 10000) {
    try {
      if (await check()) return;
    } catch {}
    await new Promise(resolveWait => setTimeout(resolveWait, 100));
  }
  throw new Error(`${label} did not become ready${appOutput ? `: ${appOutput}` : ""}`);
}

async function startApplication(cert, key, gitCA) {
  const env = { ...process.env, DEPLOY_MANAGER_TEST_GIT_CA: gitCA, DEPLOY_MANAGER_TEST_GIT_ORIGIN: fixtureOrigin };
  app = spawn(executable, ["serve", "--data", dir, "--listen", appListen, "--tls-cert", cert, "--tls-key", key, "--origin", `https://${appListen}`], { cwd: dir, env, stdio: ["ignore", "pipe", "pipe"] });
  app.stdout.on("data", chunk => { appOutput += chunk.toString(); });
  app.stderr.on("data", chunk => { appOutput += chunk.toString(); });
  await waitFor(async () => (await httpsRequestStatus("127.0.0.1", 18440, "/api/session")) === 401, "application server");
}

try {
  const binary = process.env.DEPLOY_MANAGER_BINARY;
  const fixtureBinary = process.env.DEPLOY_MANAGER_FIXTURE_BINARY;
  if (!binary || !fixtureBinary) throw new Error("combined app and smart-HTTPS fixture binaries are required");
  await copyFile(resolve(binary), executable);
  await chmod(executable, 0o700);

  const setup = spawnSync(executable, ["setup", "--data", dir, "--listen", appListen], { cwd: dir, encoding: "utf8" });
  if (setup.status !== 0) throw new Error(setup.stderr || "CLI setup failed");
  const setupURL = setup.stdout.trim();
  const grant = new URL(setupURL).hash.slice(1);
  if (!grant || !setupURL.startsWith(`http://${appListen}/register#`)) throw new Error("CLI did not issue a fragment-only local setup grant");

  // Register through the application before starting its HTTPS-only login flow.
  app = spawn(executable, ["serve", "--data", dir, "--listen", appListen], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  app.stdout.on("data", chunk => { appOutput += chunk.toString(); });
  app.stderr.on("data", chunk => { appOutput += chunk.toString(); });
  await waitFor(async () => {
    try { return (await fetch(`http://${appListen}/register`, { headers: { Accept: "text/html" } })).status === 200; } catch { return false; }
  }, "registration server");

  const cert = join(dir, "app.crt");
  const key = join(dir, "app.key");
  const certificate = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert, "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1", "-days", "1"], { encoding: "utf8" });
  if (certificate.status !== 0) throw new Error(certificate.stderr || "Could not create app TLS certificate");
  const fixtureCert = join(dir, "fixture.crt");
  const fixtureKey = join(dir, "fixture.key");
  const fixtureCertificate = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", fixtureKey, "-out", fixtureCert, "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1", "-days", "1"], { encoding: "utf8" });
  if (fixtureCertificate.status !== 0) throw new Error(fixtureCertificate.stderr || "Could not create fixture TLS certificate");

  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || chromium.executablePath(), args: ["--no-sandbox"] });
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", error => pageErrors.push(error.message));

  await page.goto(setupURL, { waitUntil: "domcontentloaded" });
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByText("Administrator account created. You can close this page.").waitFor({ timeout: 60000 });
  app.kill("SIGTERM");
  await once(app, "exit");
  app = null;

  fixture = spawn(resolve(fixtureBinary), ["--listen", fixtureListen, "--tls-cert", fixtureCert, "--tls-key", fixtureKey, "--data", join(dir, "fixture-data")], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  fixture.stderr.on("data", chunk => { appOutput += chunk.toString(); });
  await waitFor(async () => (await httpsRequestStatus("127.0.0.1", 18441, "/")) === 404, "smart-HTTPS Git fixture");
  await startApplication(cert, key, fixtureCert);

  await page.goto(`https://${appListen}/login`, { waitUntil: "domcontentloaded" });
  const unauthenticated = await page.evaluate(async () => (await fetch("/api/projects")).status);
  if (unauthenticated !== 401) throw new Error(`unauthenticated project API read returned ${unauthenticated}`);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByText(/You’re signed in/).waitFor({ timeout: 30000 });
  await page.getByLabel("Project name").fill("Fixture site");
  await page.getByLabel("HTTPS repository URL").fill(fixtureURL);
  const createResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const created = await createResponse;
  if (created.status() !== 201) throw new Error(`project creation returned ${created.status()}: ${await created.text()}\n${appOutput}`);
  await page.getByRole("heading", { name: "Branches and tags" }).waitFor({ timeout: 30000 });
  await page.getByText("main", { exact: true }).waitFor();
  await page.getByText("feature/search", { exact: true }).waitFor();
  await page.getByText("v1.0", { exact: true }).waitFor();

  const id = await page.evaluate(async () => (await (await fetch("/api/projects")).json())[0].id);
  const initialTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  const search = page.getByLabel("Search branches and tags");
  await search.fill("feature/search");
  await page.getByText("feature/search", { exact: true }).waitFor();
  if (await page.getByText("v1.0", { exact: true }).count()) throw new Error("branch search left a non-matching tag visible");
  await search.fill("v1.0");
  await page.getByText("v1.0", { exact: true }).waitFor();
  if (await page.getByText("feature/search", { exact: true }).count()) throw new Error("tag search left a non-matching branch visible");
  await search.fill("");

  const refreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh" }).click();
  const refreshed = await refreshResponse;
  if (refreshed.status() !== 200) throw new Error(`successful refresh returned ${refreshed.status()}`);
  await page.getByText("feature/search", { exact: true }).waitFor();
  const refreshedTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  if (!refreshedTimestamp || refreshedTimestamp === initialTimestamp) throw new Error("successful refresh did not update the last-fetch timestamp");

  await page.route("**/api/projects", async route => {
    if (route.request().method() === "GET" && new URL(route.request().url()).pathname === "/api/projects") {
      await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE INTERNAL RESPONSE DETAIL" });
      return;
    }
    await route.continue();
  });
  const failedReload = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "GET" && response.status() === 500);
  const refreshWithFailedListReload = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh" }).click();
  const refreshedBeforeReloadFailure = await refreshWithFailedListReload;
  if (refreshedBeforeReloadFailure.status() !== 200) throw new Error(`refresh before failed list reload returned ${refreshedBeforeReloadFailure.status()}`);
  await failedReload;
  await page.getByText(/Your current branches and tags are still shown/).waitFor();
  await page.getByText("Fixture site", { exact: true }).waitFor();
  await page.getByText("feature/search", { exact: true }).waitFor();
  if ((await page.locator("body").innerText()).includes("SENSITIVE INTERNAL RESPONSE DETAIL")) throw new Error("dashboard exposed an HTTP response body");
  await page.unroute("**/api/projects");
  const timestampAfterReloadFailure = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  if (!timestampAfterReloadFailure || timestampAfterReloadFailure === refreshedTimestamp) throw new Error("server did not record the successful refresh before the failed list reload");

  const failFixture = await httpsRequestStatus("127.0.0.1", 18441, "/__fixture/fail", "POST");
  if (failFixture !== 204) throw new Error(`fixture fail control returned ${failFixture}`);
  const cachedRefs = await page.evaluate(async id => await (await fetch(`/api/projects/${id}/refs`)).json(), id);
  const failedRefreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh" }).click();
  const failedRefresh = await failedRefreshResponse;
  if (failedRefresh.status() !== 502) throw new Error(`failed refresh returned ${failedRefresh.status()}`);
  await page.getByText(/Your current branches and tags are still shown/).waitFor();
  const retainedRefs = await page.evaluate(async id => await (await fetch(`/api/projects/${id}/refs`)).json(), id);
  if (JSON.stringify(retainedRefs) !== JSON.stringify(cachedRefs)) throw new Error("failed refresh changed the cached refs");
  const failedTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  if (failedTimestamp !== timestampAfterReloadFailure) throw new Error("failed refresh changed last successful fetch time");
  if (pageErrors.length) throw new Error(`browser errors: ${pageErrors.slice(0, 4).join("; ")}`);
  console.log("combined-binary authenticated project workflow passed: smart HTTPS create/fetch, searchable full-ID refs, refresh timestamp, and failed-refresh cache retention");
} finally {
  await browser?.close();
  for (const child of [app, fixture]) {
    if (child && child.exitCode === null) {
      child.kill("SIGTERM");
      await Promise.race([once(child, "exit"), new Promise(resolveExit => setTimeout(resolveExit, 1000))]);
    }
  }
  await rm(dir, { recursive: true, force: true });
}
