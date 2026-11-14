import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { chmod, copyFile, mkdtemp, readFile, rm, stat } from "node:fs/promises";
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
let sshFixture;
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
  const env = { ...process.env, DEPLOY_MANAGER_TEST_GIT_CA: gitCA, DEPLOY_MANAGER_TEST_GIT_ORIGIN: fixtureOrigin, DEPLOY_MANAGER_TEST_LFS_CA: gitCA };
  app = spawn(executable, ["serve", "--data", dir, "--listen", appListen, "--tls-cert", cert, "--tls-key", key, "--origin", `https://${appListen}`], { cwd: dir, env, stdio: ["ignore", "pipe", "pipe"] });
  app.stdout.on("data", chunk => { appOutput += chunk.toString(); });
  app.stderr.on("data", chunk => { appOutput += chunk.toString(); });
  await waitFor(async () => (await httpsRequestStatus("127.0.0.1", 18440, "/api/session")) === 401, "application server");
}

try {
  const binary = process.env.DEPLOY_MANAGER_BINARY;
  const fixtureBinary = process.env.DEPLOY_MANAGER_FIXTURE_BINARY;
  const sshFixtureBinary = process.env.DEPLOY_MANAGER_SSH_FIXTURE_BINARY;
  if (!binary || !fixtureBinary || !sshFixtureBinary) throw new Error("combined app, smart-HTTPS fixture, and SSH fixture binaries are required");
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
  sshFixture = spawn(resolve(sshFixtureBinary), ["--listen", "127.0.0.1:18442", "--lfs-href", `${fixtureOrigin}/private-lfs`], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  sshFixture.stderr.on("data", chunk => { appOutput += chunk.toString(); });
  await waitFor(async () => {
    try { const socket = await import("node:net").then(({ createConnection }) => new Promise(resolveSocket => { const client = createConnection({ host: "127.0.0.1", port: 18442 }); client.once("connect", () => { client.destroy(); resolveSocket(true); }); client.once("error", () => resolveSocket(false)); })); return socket; } catch { return false; }
  }, "SSH Git fixture");
  await startApplication(cert, key, fixtureCert);

  await page.goto(`https://${appListen}/login`, { waitUntil: "domcontentloaded" });
  const unauthenticated = await page.evaluate(async () => (await fetch("/api/projects")).status);
  if (unauthenticated !== 401) throw new Error(`unauthenticated project API read returned ${unauthenticated}`);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByText(/You’re signed in/).waitFor({ timeout: 30000 });
  const sessionCookie = (await context.cookies()).find(cookie => cookie.name === "__Host-deploy-manager-session")?.value;
  if (!sessionCookie) throw new Error("authenticated browser session cookie is missing");
  const crossOriginMutation = await httpsRequestStatus("127.0.0.1", 18440, "/api/ssh-hosts/approve", "POST", JSON.stringify({ host: "127.0.0.1", port: 18442, fingerprint: "SHA256:invalid", confirm: true }), { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" });
  if (crossOriginMutation !== 403) throw new Error(`cross-origin SSH approval returned ${crossOriginMutation}`);
  await page.getByLabel("Project name").fill("Fixture site");
  await page.getByLabel("Repository URL").fill(fixtureURL);
  const createResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const created = await createResponse;
  if (created.status() !== 201) throw new Error(`project creation returned ${created.status()}: ${await created.text()}\n${appOutput}`);
  await page.getByRole("heading", { name: "Branches and tags" }).waitFor({ timeout: 30000 });
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  await page.getByText("v1.0", { exact: true }).waitFor();

  await page.getByText("Fixture site", { exact: true }).click();
  await page.getByLabel("LFS server URL").waitFor();
  await page.getByLabel("LFS server URL").fill(`${fixtureURL}/info/lfs`);
  await page.getByLabel("Object download origins").fill(fixtureOrigin);
  await page.getByLabel("Allow downloads from internal network addresses").check();
  await page.getByLabel("I independently verified the internal destinations and intend to allow them.").check();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Save LFS settings" }).click();
  await page.getByText("LFS settings saved.").waitFor();

  const id = await page.evaluate(async () => (await (await fetch("/api/projects")).json())[0].id);
  await page.getByLabel("Deployment name").fill("Production site");
  await page.locator("form").locator("select").selectOption("branch:feature/search");
  const deploymentResponse = page.waitForResponse(response => response.url().endsWith("/api/deployments") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create deployment" }).click();
  const deploymentCreated = await deploymentResponse;
  if (deploymentCreated.status() !== 201) throw new Error(`deployment creation returned ${deploymentCreated.status()}: ${await deploymentCreated.text()}`);
  await page.getByText("Production site", { exact: true }).waitFor();
  const deploymentID = await page.evaluate(async () => (await (await fetch("/api/deployments")).json())[0].id);
  try {
  await waitFor(async () => {
      try {
        const published = join(dir, "state.sqlite.publications", deploymentID);
        return (await readFile(join(published, "index.html"), "utf8")) === "<!doctype html><title>feature search</title>\n" && (await readFile(join(published, "asset.dat"), "utf8")) === "fixture hydrated LFS asset\n";
      } catch { return false; }
    }, "published deployment files");
  } catch (error) {
    const state = await page.evaluate(async id => ({ deployment: await (await fetch("/api/deployments")).text(), jobs: await (await fetch(`/api/deployments/${id}/jobs`)).text() }), deploymentID);
    throw new Error(`${error.message}; state: ${JSON.stringify(state)}`);
  }
  await page.getByText("Live", { exact: true }).waitFor();
  const publishedPath = join(dir, "state.sqlite.publications", deploymentID);
  const firstDirectoryIdentity = (await stat(publishedPath)).ino;
  const mainCommit = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).find(ref => ref.kind === "branch" && ref.name === "main").commit, id);
  await page.getByLabel("Target for Production site").selectOption("branch:main");
  const targetUpdate = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/target`));
  await page.getByRole("button", { name: "Change target" }).click();
  if ((await targetUpdate).status() !== 202) throw new Error("changing the deployment branch did not queue publication");
  await waitFor(async () => {
    try { return (await readFile(join(publishedPath, "index.html"), "utf8")) === "<!doctype html><title>main</title>\n"; } catch { return false; }
  }, "replacement publication files");
  if ((await stat(publishedPath)).ino === firstDirectoryIdentity) throw new Error("replacement did not switch the live directory");
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  await page.getByText(/Latest deployment: noop/).waitFor({ timeout: 10000 });

  const initialTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  const search = page.getByLabel("Search branches and tags");
  await search.fill("feature/search");
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  if (await page.getByText("v1.0", { exact: true }).count()) throw new Error("branch search left a non-matching tag visible");
  await search.fill("v1.0");
  await page.getByText("v1.0", { exact: true }).waitFor();
  if (await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).count()) throw new Error("tag search left a non-matching branch visible");
  await search.fill("");

  await page.getByLabel("Deployment name").fill("Tag release");
  await page.locator("form select").selectOption("tag:v1.0");
  const tagDeploymentResponse = page.waitForResponse(response => response.url().endsWith("/api/deployments") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create deployment" }).click();
  const tagCreated = await tagDeploymentResponse;
  if (tagCreated.status() !== 201) throw new Error(`tag deployment creation returned ${tagCreated.status()}: ${await tagCreated.text()}`);
  const tagDeployment = await tagCreated.json();
  const tagPath = join(dir, "state.sqlite.publications", tagDeployment.id);
  await waitFor(async () => {
    try { return (await readFile(join(tagPath, "index.html"), "utf8")) === "<!doctype html><title>feature search</title>\n"; } catch { return false; }
  }, "tag-target publication from the dashboard");
  const tagCard = page.locator("li").filter({ hasText: "Tag release" });
  const tagIdentity = (await stat(tagPath)).ino;
  const hardRedeployResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${tagDeployment.id}/hard-redeploy`));
  await tagCard.getByRole("button", { name: "Hard re-deploy", exact: true }).click();
  if ((await hardRedeployResponse).status() !== 202) throw new Error("dashboard Hard re-deploy was not queued");
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), tagDeployment.id);
    return jobs[0]?.status === "succeeded" && (await stat(tagPath)).ino !== tagIdentity;
  }, "dashboard Hard re-deploy to recreate the tag publication");

  const refreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh branches and tags", exact: true }).click();
  const refreshed = await refreshResponse;
  if (refreshed.status() !== 200) throw new Error(`successful refresh returned ${refreshed.status()}`);
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
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
  await page.getByRole("button", { name: "Refresh branches and tags", exact: true }).click();
  const refreshedBeforeReloadFailure = await refreshWithFailedListReload;
  if (refreshedBeforeReloadFailure.status() !== 200) throw new Error(`refresh before failed list reload returned ${refreshedBeforeReloadFailure.status()}`);
  await failedReload;
  await page.getByText(/Your current branches and tags are still shown/).waitFor();
  await page.getByText("Fixture site", { exact: true }).waitFor();
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  if ((await page.locator("body").innerText()).includes("SENSITIVE INTERNAL RESPONSE DETAIL")) throw new Error("dashboard exposed an HTTP response body");
  await page.unroute("**/api/projects");
  const timestampAfterReloadFailure = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  if (!timestampAfterReloadFailure || timestampAfterReloadFailure === refreshedTimestamp) throw new Error("server did not record the successful refresh before the failed list reload");

  const failFixture = await httpsRequestStatus("127.0.0.1", 18441, "/__fixture/fail", "POST");
  if (failFixture !== 204) throw new Error(`fixture fail control returned ${failFixture}`);
  const cachedRefs = await page.evaluate(async id => await (await fetch(`/api/projects/${id}/refs`)).json(), id);
  const failedRefreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh branches and tags", exact: true }).click();
  const failedRefresh = await failedRefreshResponse;
  if (failedRefresh.status() !== 502) throw new Error(`failed refresh returned ${failedRefresh.status()}`);
  await page.getByText(/Your current branches and tags are still shown/).waitFor();
  const retainedRefs = await page.evaluate(async id => await (await fetch(`/api/projects/${id}/refs`)).json(), id);
  if (JSON.stringify(retainedRefs) !== JSON.stringify(cachedRefs)) throw new Error("failed refresh changed the cached refs");
  const failedTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).find(project => project.id === id).lastFetched, id);
  if (failedTimestamp !== timestampAfterReloadFailure) throw new Error("failed refresh changed last successful fetch time");

  await page.getByLabel("Key name").fill("SSH browser key");
  await page.getByRole("button", { name: "Create Ed25519 key" }).click();
  await page.getByText("SSH browser key", { exact: true }).waitFor();
  const sshKeyId = await page.evaluate(async () => (await (await fetch("/api/repository-keys")).json()).find(key => key.name === "SSH browser key").id);
  await page.getByLabel("Project name").fill("SSH fixture site");
  await page.getByLabel("Repository URL").fill("ssh://git@127.0.0.1:18442/repo.git");
  await page.getByLabel("SSH key").selectOption(sshKeyId);
  const blockedCreateResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const blockedCreate = await blockedCreateResponse;
  if (blockedCreate.status() !== 409) throw new Error(`unknown SSH host was not blocked (${blockedCreate.status()})`);
  const hostTrust = page.locator("section").filter({ has: page.getByRole("heading", { name: "SSH host keys" }) });
  await hostTrust.getByText(/SHA256:/).waitFor();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Approve verified host key" }).click();
  await hostTrust.getByText("No SSH host warnings.").waitFor();
  const sshCreateResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const sshCreated = await sshCreateResponse;
  if (sshCreated.status() !== 201) throw new Error(`approved SSH project creation returned ${sshCreated.status()}: ${await sshCreated.text()}`);
  await page.getByText("SSH fixture site", { exact: true }).waitFor();
  const sshProject = await page.evaluate(async () => (await (await fetch("/api/projects")).json()).find(project => project.name === "SSH fixture site"));
  const sshRefs = await page.evaluate(async id => await (await fetch(`/api/projects/${id}/refs`)).json(), sshProject.id);
  if (!sshRefs.some(ref => ref.kind === "branch" && ref.name === "main")) throw new Error("approved SSH project did not cache its branch");
  await page.getByText("SSH fixture site", { exact: true }).click();
  await page.getByLabel("LFS server URL").fill(`${fixtureOrigin}/private-lfs`);
  await page.getByLabel("Object download origins").fill(fixtureOrigin);
  await page.getByLabel("Allow downloads from internal network addresses").check();
  await page.getByLabel("I independently verified the internal destinations and intend to allow them.").check();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Save LFS settings" }).click();
  await page.getByText("LFS settings saved.").waitFor();
  const sshCommit = sshRefs.find(ref => ref.kind === "branch" && ref.name === "main").commit;
  const sshDeployment = await page.evaluate(async ({ projectID, commit }) => {
    const response = await fetch("/api/deployments", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ projectID, name: "SSH published site", commit }) });
    return { status: response.status, body: await response.json().catch(() => null) };
  }, { projectID: sshProject.id, commit: sshCommit });
  if (sshDeployment.status !== 201) throw new Error(`SSH publication was not accepted (${sshDeployment.status})`);
  await waitFor(async () => {
    try {
      const published = join(dir, "state.sqlite.publications", sshDeployment.body.id);
      return (await readFile(join(published, "index.html"), "utf8")) === "SSH fixture site\n" && (await readFile(join(published, "asset.dat"), "utf8")) === "fixture hydrated LFS asset\n";
    } catch { return false; }
  }, "SSH cached-object publication");
  const privateJobLog = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).text(), sshDeployment.body.id);
  for (const secret of ["ssh-fixture-private-lfs", "private-fixture-secret", "Bearer"]) {
    if (privateJobLog.includes(secret) || appOutput.includes(secret)) throw new Error(`private LFS secret reached an application or job log (${secret})`);
  }
  const sshPublishedPath = join(dir, "state.sqlite.publications", sshDeployment.body.id);
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByText(/Delete “SSH published site” and remove its published files/).waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  if ((await readFile(join(sshPublishedPath, "index.html"), "utf8")) !== "SSH fixture site\n") throw new Error("cancelling deployment deletion changed its publication");
  await page.getByRole("button", { name: "Delete project" }).click();
  await page.getByText(/Projects with deployments cannot be deleted/).waitFor();
  const blockedProjectDelete = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  if ((await blockedProjectDelete).status() !== 409) throw new Error("project deletion was not blocked while a deployment existed");
  if (!(await page.evaluate(async id => (await (await fetch("/api/projects")).json()).some(project => project.id === id), sshProject.id))) throw new Error("blocked project deletion removed the project");
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const deploymentDelete = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${sshDeployment.body.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  if ((await deploymentDelete).status() !== 204) throw new Error("confirmed deployment deletion did not finish");
  await waitFor(async () => { try { await stat(sshPublishedPath); return false; } catch { return true; } }, "deleted publication files");
  await page.getByRole("button", { name: "Delete project" }).click();
  await page.getByText(/Projects with deployments cannot be deleted/).waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  if (!(await page.evaluate(async id => (await (await fetch("/api/projects")).json()).some(project => project.id === id), sshProject.id))) throw new Error("cancelling project deletion changed the project");
  await page.getByRole("button", { name: "Delete project" }).click();
  const projectDelete = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  const confirmedProjectDelete = await projectDelete;
  if (confirmedProjectDelete.status() !== 204) throw new Error(`confirmed project deletion returned ${confirmedProjectDelete.status()}: ${await confirmedProjectDelete.text()}`);
  if (await page.evaluate(async id => (await (await fetch("/api/projects")).json()).some(project => project.id === id), sshProject.id)) throw new Error("deleted project remained visible");
  try { await stat(join(dir, "state.sqlite.projects", sshProject.id)); throw new Error("project cache remains after deletion"); } catch (error) { if (error.message === "project cache remains after deletion") throw error; }
  if (pageErrors.length) throw new Error(`browser errors: ${pageErrors.slice(0, 4).join("; ")}`);
  console.log("combined-binary authenticated project workflow passed: publication and deletion confirmation/cancel, private SSH/LFS publication, host trust, cross-origin mutation rejection, and cached-ref retention");
} finally {
  await browser?.close();
  for (const child of [app, fixture, sshFixture]) {
    if (child && child.exitCode === null) {
      child.kill("SIGTERM");
      await Promise.race([once(child, "exit"), new Promise(resolveExit => setTimeout(resolveExit, 1000))]);
    }
  }
  await rm(dir, { recursive: true, force: true });
}
