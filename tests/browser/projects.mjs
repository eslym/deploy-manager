import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { createHmac, generateKeyPairSync } from "node:crypto";
import { chmod, copyFile, mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
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
const focusedOutcomesComplete = Symbol("focused deployment outcomes complete");
let app;
let fixture;
let sshFixture;
let browser;
let appOutput = "";
let releasePauseFailure;
let currentStep = "initialization";
function milestone(label) {
  currentStep = label;
  console.log(`[projects-browser] ${label}`);
}

function outcome(code, message, data = null) {
  return { code, message, data };
}

async function withDeadline(promise, label, milliseconds = 10000) {
  let timer;
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`${label} timed out`)), milliseconds); })
    ]);
  } finally {
    clearTimeout(timer);
  }
}

function httpsRequestStatus(host, port, path, method = "GET", body = "", headers = {}) {
  return new Promise((resolveStatus, reject) => {
    const request = httpsRequest({ hostname: host, port, path, method, rejectUnauthorized: false, headers }, response => {
      response.resume();
      response.on("end", () => resolveStatus(response.statusCode));
    });
    request.on("error", reject);
    request.setTimeout(5000, () => request.destroy(new Error("HTTPS fixture request timed out")));
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

async function waitForNoopHistory(page, deploymentID, card) {
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
    return jobs[0]?.status === "noop";
  }, "latest no-op deployment job");
  await card.getByRole("region", { name: "Job history for Production site" }).locator("ul > li").first().getByText("No changes", { exact: true }).waitFor();
}

async function openProject(page, id, name) {
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.getByRole("link", { name, exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}`);
}

async function assertProjectLayout(page) {
  // The original two-column layout placed storage and LFS outside the selected project's column.
  // Check the actual rendered rectangles, not just CSS class names or a screenshot snapshot.
  for (const width of [1440, 1049, 390]) {
    await page.setViewportSize({ width, height: 900 });
    const bounds = await page.evaluate(() => {
      const detail = [...document.querySelectorAll("h2")].find(node => node.textContent === "Branches and tags")?.closest("section")?.getBoundingClientRect();
      const storage = document.getElementById("storage-heading")?.closest("section")?.getBoundingClientRect();
      const actions = [...document.querySelectorAll("button")].find(node => node.textContent === "Delete project")?.getBoundingClientRect();
      const lfs = [...document.querySelectorAll("h2")].find(node => node.textContent === "Git LFS destinations")?.closest("section")?.getBoundingClientRect();
      return { detail: detail && { left: detail.left, right: detail.right }, storage: storage && { left: storage.left, right: storage.right }, actions: actions && { left: actions.left, right: actions.right }, lfs: lfs && { left: lfs.left, right: lfs.right }, hasProjectList: [...document.querySelectorAll("h2")].some(node => node.textContent === "Your projects"), scrollWidth: document.documentElement.scrollWidth, width: window.innerWidth };
    });
    if (!bounds.detail || !bounds.storage || !bounds.actions || !bounds.lfs) throw new Error(`missing project detail section at ${width}px: ${JSON.stringify(bounds)}`);
    if (bounds.hasProjectList || [bounds.storage, bounds.actions, bounds.lfs].some(rect => rect.left < bounds.detail.left - 2 || rect.right > bounds.detail.right + 2) || bounds.scrollWidth > bounds.width) throw new Error(`project detail escaped its dedicated page at ${width}px: ${JSON.stringify(bounds)}`);
  }
  await page.setViewportSize({ width: 1280, height: 900 });
}

async function startApplication(cert, key, gitCA) {
  const env = { ...process.env, DEPLOY_MANAGER_TEST_GIT_CA: gitCA, DEPLOY_MANAGER_TEST_GIT_ORIGIN: fixtureOrigin, DEPLOY_MANAGER_TEST_LFS_CA: gitCA };
  app = spawn(executable, ["serve", "--data", dir, "--listen", appListen, "--tls-cert", cert, "--tls-key", key, "--origin", `https://${appListen}`, "--auth-rate-per-minute", "1000"], { cwd: dir, env, stdio: ["ignore", "pipe", "pipe"] });
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
  app = spawn(executable, ["serve", "--data", dir, "--listen", appListen, "--auth-rate-per-minute", "1000"], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
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
  context.setDefaultTimeout(10000);
  context.setDefaultNavigationTimeout(15000);
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", error => pageErrors.push(error.message));

  milestone("register administrator");
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

  milestone("sign in over HTTPS");
  await page.goto(`https://${appListen}/login`, { waitUntil: "domcontentloaded" });
  const unauthenticated = await page.evaluate(async () => (await fetch("/api/projects")).status);
  if (unauthenticated !== 401) throw new Error(`unauthenticated project API read returned ${unauthenticated}`);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.getByText(/You’re signed in/).waitFor({ timeout: 30000 });
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.waitForURL("**/dashboard/projects");
  milestone("approve SSH host while adding a repository");
  await page.route("**/api/repository-keys", route => route.fulfill({
    status: 200,
    contentType: "application/json",
    body: JSON.stringify({ code: "repository_key.listed", message: "Repository keys loaded.", data: [{ id: "fixture-key", name: "Fixture key", algorithm: "ed25519", bits: 256, fingerprint: "SHA256:test", publicKey: "ssh-ed25519 test", createdAt: new Date().toISOString() }] })
  }));
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByLabel("Project name").fill("SSH fixture site");
  await page.getByLabel("Repository URL").fill("git@host.example:owner/site.git");
  await page.getByRole("link", { name: "review SSH host warnings" }).waitFor();
  const sshKeySelect = page.getByLabel("SSH key");
  await sshKeySelect.locator("option").nth(1).waitFor({ state: "attached" });
  await sshKeySelect.selectOption({ index: 1 });
  const observedHost = {
    host: "host.example",
    port: 22,
    fingerprint: "SHA256:observed-key-fingerprint",
    warning: "SSH host key is not approved; verify the fingerprint independently before approval",
    trusted: false
  };
  let projectAttempts = 0;
  const projectRequests = [];
  let approvalAttempts = 0;
  let latestWarning = observedHost;
  let hideWarningAfterApprovalFailure = false;
  let warningDisappearsAfterApprovalFailure = false;
  await page.route("**/api/ssh-hosts", async route => {
    if (route.request().method() === "GET") {
      const hosts = [
        { host: observedHost.host, port: observedHost.port, fingerprint: "SHA256:trusted-key", warning: "", trusted: true },
        { ...latestWarning, host: "other.example", port: 22 },
        latestWarning
      ];
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ code: "ssh_host.listed", message: "SSH hosts loaded.", data: warningDisappearsAfterApprovalFailure
          ? hosts.filter(host => host.host !== observedHost.host || !host.warning)
          : hosts })
      });
      return;
    }
    await route.continue();
  });
  await page.route("**/api/ssh-hosts/approve", async route => {
    approvalAttempts++;
    if (approvalAttempts === 1) {
      latestWarning = { ...observedHost, fingerprint: "SHA256:replacement-key-fingerprint" };
      if (hideWarningAfterApprovalFailure) warningDisappearsAfterApprovalFailure = true;
      await route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ code: "ssh_host.observation_changed", message: "SSH host details changed. Review them again.", data: null }) });
      return;
    }
    const approval = route.request().postDataJSON();
    if (approval.fingerprint !== latestWarning.fingerprint || approval.host !== observedHost.host || approval.port !== observedHost.port || approval.confirm !== true) {
      throw new Error(`approval did not match the reviewed observation: ${JSON.stringify(approval)}`);
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(outcome("ssh_host.approved", "SSH host approved.")) });
  });
  await page.route("**/api/projects", async route => {
    if (route.request().method() !== "POST") return route.continue();
    projectAttempts++;
    projectRequests.push(route.request().postDataJSON());
    if (projectAttempts <= 2) {
      await route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify(outcome("ssh_host.approval_required", "Verify and approve the SSH host first.")) });
      return;
    }
    await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify(outcome("project.created", "Project created.", { id: "ssh-browser-project", name: "SSH fixture site", url: "git@host.example:owner/site.git", keyId: "fixture-key", lastFetched: null, gitCacheBytes: 0, lfsCacheBytes: 0 })) });
  });
  const createButton = page.getByRole("button", { name: "Add repository" });
  await createButton.click();
  const approvalDialog = page.getByRole("dialog");
  await approvalDialog.waitFor();
  await approvalDialog.getByText("host.example", { exact: true }).waitFor();
  await approvalDialog.getByText("22", { exact: true }).waitFor();
  await approvalDialog.getByText(observedHost.fingerprint, { exact: true }).waitFor();
  await approvalDialog.getByText(/independently/i).waitFor();
  await approvalDialog.getByRole("button", { name: "Cancel" }).click();
  if (projectAttempts !== 1 || approvalAttempts !== 0) throw new Error("canceling host approval created a project or approved a key");
  if (await page.getByLabel("Project name").inputValue() !== "SSH fixture site" || await page.getByLabel("Repository URL").inputValue() !== "git@host.example:owner/site.git") {
    throw new Error("canceling host approval cleared the project form");
  }
  await createButton.click();
  await page.getByRole("dialog").getByRole("button", { name: /Approve/ }).click();
  await page.getByRole("dialog").getByText("SHA256:replacement-key-fingerprint", { exact: true }).waitFor();
  await page.getByRole("dialog").getByRole("alert").filter({ hasText: /changed|review/i }).waitFor();
  if (projectAttempts !== 2) throw new Error("project creation retried before the changed fingerprint was reviewed");
  await page.getByRole("dialog").getByRole("button", { name: /Approve/ }).click();
  await page.waitForURL("**/dashboard/projects/ssh-browser-project");
  if (projectAttempts !== 3 || approvalAttempts !== 2) throw new Error(`approval did not retry project creation: projects=${projectAttempts}, approvals=${approvalAttempts}`);
  if (projectRequests.some(body => body.name !== "SSH fixture site" || body.url !== "git@host.example:owner/site.git" || body.keyId !== "fixture-key")) {
    throw new Error(`approval retry did not preserve the project form: ${JSON.stringify(projectRequests)}`);
  }
  milestone("dismiss stale host approval when warning disappears");
  await page.goto(`https://${appListen}/dashboard/projects`, { waitUntil: "domcontentloaded" });
  await page.getByLabel("Project name").fill("SSH fixture site");
  await page.getByLabel("Repository URL").fill("git@host.example:owner/site.git");
  await page.getByLabel("SSH key").selectOption({ index: 1 });
  projectAttempts = 0;
  approvalAttempts = 0;
  latestWarning = observedHost;
  hideWarningAfterApprovalFailure = true;
  warningDisappearsAfterApprovalFailure = false;
  await page.getByRole("button", { name: "Add repository" }).click();
  const staleApprovalDialog = page.getByRole("dialog");
  await staleApprovalDialog.waitFor();
  if (projectAttempts !== 1) throw new Error("SSH create did not first request the project before showing host approval");
  await staleApprovalDialog.getByRole("button", { name: /Approve/ }).click();
  await page.getByRole("alert").filter({ hasText: /host warning is no longer available/i }).waitFor();
  if (await page.getByRole("dialog").count()) throw new Error("stale host fingerprint remained approvable after its warning disappeared");
  if (approvalAttempts !== 1 || projectAttempts !== 1) throw new Error(`missing-warning recovery retried approval or project creation: projects=${projectAttempts}, approvals=${approvalAttempts}`);
  if (await page.getByLabel("Project name").inputValue() !== "SSH fixture site" || await page.getByLabel("Repository URL").inputValue() !== "git@host.example:owner/site.git") {
    throw new Error("missing-warning recovery cleared the project form");
  }
  await page.unroute("**/api/projects");
  await page.unroute("**/api/repository-keys");
  await page.unroute("**/api/ssh-hosts");
  await page.unroute("**/api/ssh-hosts/approve");
  await page.goto(`https://${appListen}/dashboard/projects`, { waitUntil: "domcontentloaded" });
  const sessionCookie = (await context.cookies()).find(cookie => cookie.name === "__Host-deploy-manager-session")?.value;
  if (!sessionCookie) throw new Error("authenticated browser session cookie is missing");
  const crossOriginMutation = await httpsRequestStatus("127.0.0.1", 18440, "/api/ssh-hosts/approve", "POST", JSON.stringify({ host: "127.0.0.1", port: 18442, fingerprint: "SHA256:invalid", confirm: true }), { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" });
  if (crossOriginMutation !== 403) throw new Error(`cross-origin SSH approval returned ${crossOriginMutation}`);
  milestone("create HTTPS project and retry webhook settings");
  await page.getByLabel("Project name").fill("Fixture site");
  await page.getByLabel("Repository URL").fill(fixtureURL);
  let webhookSettingsGets = 0;
  let webhookRotationPosts = 0;
  await page.route("**/api/projects/*/github-webhook", async route => {
    if (route.request().method() === "GET") {
      if (webhookSettingsGets++ === 0) {
        await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "RAW_WEBHOOK_ERROR_SENTINEL" }) });
        return;
      }
    } else if (route.request().method() === "POST") {
      webhookRotationPosts++;
    }
    await route.continue();
  });
  const createResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const created = await createResponse;
  if (created.status() !== 201) throw new Error(`project creation returned ${created.status()}: ${await created.text()}\n${appOutput}`);
  const createdOutcome = await created.json();
  if (createdOutcome.code !== "project.created") throw new Error(`project creation returned ${JSON.stringify(createdOutcome)}`);
  const id = createdOutcome.data.id;
  await page.waitForURL(`**/dashboard/projects/${id}`);
  await page.getByRole("heading", { name: "Fixture site", exact: true }).waitFor();
  if (!(await page.title()).includes("Fixture site")) throw new Error("project detail lacks its own page title");
  await page.getByRole("heading", { name: "Branches and tags" }).waitFor({ timeout: 30000 });
  await page.getByRole("alert").filter({ hasText: "The request could not be completed. Try again." }).waitFor();
  const dashboardURLBeforeRetry = page.url();
  const githubWebhook = page.getByRole("region", { name: "GitHub webhook", exact: true });
  await githubWebhook.getByRole("button", { name: "Retry loading settings" }).click();
  await githubWebhook.getByRole("button", { name: "Set up webhook", exact: true }).waitFor();
  if (await page.getByRole("alert").count()) throw new Error("the webhook settings load error remained after a successful retry");
  if (webhookSettingsGets !== 2) throw new Error(`webhook settings were requested ${webhookSettingsGets} times, expected one retry after the failed load`);
  if (webhookRotationPosts !== 0) throw new Error("retrying settings unexpectedly rotated the webhook secret");
  if (page.url() !== dashboardURLBeforeRetry) throw new Error("retrying webhook settings navigated away from the dashboard");
  if ((await page.locator("body").innerText()).includes("RAW_WEBHOOK_ERROR_SENTINEL")) throw new Error("raw webhook error response was shown in the dashboard");
  await page.unroute("**/api/projects/*/github-webhook");
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Fixture site", exact: true }).waitFor();
  await assertProjectLayout(page);
  const projectRefresh = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh project branches and tags" }).click();
  if ((await projectRefresh).status() !== 200) throw new Error("project detail refresh failed");
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  await page.getByText("v1.0", { exact: true }).waitFor();

  milestone("configure LFS");
  await page.getByLabel("LFS server URL").waitFor();
  const initialLfsSettings = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/lfs`)).json()).data, id);
  if (!Array.isArray(initialLfsSettings.settings.objectOrigins) || initialLfsSettings.settings.objectOrigins.length !== 0) throw new Error(`unconfigured LFS settings had an inconsistent shape: ${JSON.stringify(initialLfsSettings)}`);
  await page.getByLabel("LFS server URL").fill(`${fixtureURL}/info/lfs`);
  await page.getByLabel("Object download origins").fill(fixtureOrigin);
  await page.getByLabel("Allow downloads from internal network addresses").check();
  await page.getByLabel("I independently verified the internal destinations and intend to allow them.").check();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Save LFS settings" }).click();
  await page.getByText("LFS settings saved.").waitFor();
  const configuredLfsSettings = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/lfs`)).json()).data, id);
  if (JSON.stringify(configuredLfsSettings.settings.objectOrigins) !== JSON.stringify([fixtureOrigin])) throw new Error(`configured LFS destinations were not rendered consistently: ${JSON.stringify(configuredLfsSettings.settings)}`);

  milestone("ordinary repository LFS settings and stale response protection");
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.getByLabel("Project name").fill("Ordinary fixture site");
  await page.getByLabel("Repository URL").fill(`${fixtureOrigin}/ordinary.git`);
  const ordinaryCreateResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const ordinaryCreated = await ordinaryCreateResponse;
  if (ordinaryCreated.status() !== 201) throw new Error(`ordinary project creation returned ${ordinaryCreated.status()}: ${await ordinaryCreated.text()}`);
  const ordinaryOutcome = await ordinaryCreated.json();
  if (ordinaryOutcome.code !== "project.created") throw new Error(`ordinary project creation returned ${JSON.stringify(ordinaryOutcome)}`);
  const ordinaryProject = ordinaryOutcome.data;
  await page.waitForURL(`**/dashboard/projects/${ordinaryProject.id}`);
  const ordinaryRefsOutcome = await page.evaluate(async projectID => (await (await fetch(`/api/projects/${projectID}/refs`)).json()), ordinaryProject.id);
  if (ordinaryRefsOutcome.code !== "project.refs_listed") throw new Error(`cached refs returned ${JSON.stringify(ordinaryRefsOutcome)}`);
  const ordinaryRefs = ordinaryRefsOutcome.data;
  if (!ordinaryRefs.some(ref => ref.kind === "branch" && ref.name === "main")) throw new Error("ordinary fixture repository did not have a main branch");
  await page.getByLabel("LFS server URL").waitFor();
  const ordinarySettings = await page.evaluate(async projectID => (await (await fetch(`/api/projects/${projectID}/lfs`)).json()).data, ordinaryProject.id);
  if (!Array.isArray(ordinarySettings.settings.objectOrigins) || ordinarySettings.settings.objectOrigins.length !== 0) throw new Error(`ordinary repository LFS settings had an inconsistent shape: ${JSON.stringify(ordinarySettings)}`);
  if (await page.getByLabel("LFS server URL").inputValue() !== "" || await page.getByLabel("Object download origins").inputValue() !== "") throw new Error("ordinary repository displayed unexpected LFS destinations");

  let releaseLfsResponse;
  const delayedLfsResponse = new Promise(resolveResponse => { releaseLfsResponse = resolveResponse; });
  let interceptedLfsResponse;
  const lfsResponseIntercepted = new Promise(resolveIntercepted => { interceptedLfsResponse = resolveIntercepted; });
  await page.route(`**/api/projects/${id}/lfs`, async route => {
    if (route.request().method() === "GET") {
      interceptedLfsResponse(route);
      await delayedLfsResponse;
      return;
    }
    await route.continue();
  });
  await openProject(page, id, "Fixture site");
  const heldLfsResponse = await lfsResponseIntercepted;
  await openProject(page, ordinaryProject.id, "Ordinary fixture site");
  await page.getByLabel("LFS server URL").waitFor();
  if (await page.getByLabel("LFS server URL").inputValue() !== "" || await page.getByLabel("Object download origins").inputValue() !== "") throw new Error("switching to an ordinary repository showed another project's LFS settings");
  releaseLfsResponse();
  await heldLfsResponse.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ code: "lfs.settings_loaded", message: "LFS settings loaded.", data: { settings: { endpoint: "https://stale.example/lfs", objectOrigins: ["https://stale.example"], allowInternal: false }, suggestedEndpoint: "https://stale.example/lfs" } }) });
  await page.waitForTimeout(100);
  if (await page.getByLabel("LFS server URL").inputValue() !== "" || await page.getByLabel("Object download origins").inputValue() !== "") throw new Error("a delayed prior-project response overwrote the selected project's LFS form");
  await page.unroute(`**/api/projects/${id}/lfs`);
  await openProject(page, id, "Fixture site");
  await page.getByLabel("LFS server URL").waitFor();
  let releaseLfsSave;
  const delayedLfsSave = new Promise(resolveSave => { releaseLfsSave = resolveSave; });
  let interceptedLfsSave;
  const lfsSaveIntercepted = new Promise(resolveIntercepted => { interceptedLfsSave = resolveIntercepted; });
  await page.route(`**/api/projects/${id}/lfs`, async route => {
    if (route.request().method() === "POST") {
      interceptedLfsSave(route);
      await delayedLfsSave;
      return;
    }
    await route.continue();
  });
  await page.getByLabel("LFS server URL").fill(`${fixtureOrigin}/info/lfs`);
  await page.getByLabel("Object download origins").fill(fixtureOrigin);
  await page.getByLabel("I independently verified the internal destinations and intend to allow them.").check();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Save LFS settings" }).click();
  const heldLfsSave = await lfsSaveIntercepted;
  await openProject(page, ordinaryProject.id, "Ordinary fixture site");
  releaseLfsSave();
  await heldLfsSave.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ code: "lfs.settings_saved", message: "LFS settings saved.", data: null }) });
  await page.waitForTimeout(100);
  if (await page.getByLabel("LFS server URL").inputValue() !== "" || await page.getByLabel("Object download origins").inputValue() !== "") throw new Error("a completed save for another project overwrote the selected project's LFS form");
  if (await page.getByText("LFS settings saved.", { exact: true }).count()) throw new Error("a completed save for another project showed a stale success notice");
  await page.unroute(`**/api/projects/${id}/lfs`);
  await openProject(page, id, "Fixture site");
  await page.getByLabel("LFS server URL").waitFor();

  if (process.env.PROJECT_OUTCOMES_ONLY === "true") {
    milestone("exercise project outcome operations against the combined binary");
    const cachedRefsBeforeFailure = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data, id);
    if (await httpsRequestStatus("127.0.0.1", 18441, "/__fixture/fail", "POST") !== 204) throw new Error("could not make the Git server unavailable for the focused refresh failure");
    const failedRefreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
    await page.getByRole("button", { name: "Refresh project branches and tags" }).click();
    const failedRefresh = await failedRefreshResponse;
    const failedRefreshOutcome = await failedRefresh.json();
    if (failedRefresh.status() !== 502 || failedRefreshOutcome.code !== "operation.upstream_failed") throw new Error(`upstream refresh failure returned ${JSON.stringify(failedRefreshOutcome)}`);
    const refsAfterFailure = await page.evaluate(async projectID => (await (await fetch(`/api/projects/${projectID}/refs`)).json()).data, id);
    if (JSON.stringify(refsAfterFailure) !== JSON.stringify(cachedRefsBeforeFailure)) throw new Error("failed refresh changed cached refs");
    const cacheSection = page.getByRole("region", { name: "Repository storage" });
    await cacheSection.getByRole("button", { name: "Clear cache…" }).click();
    const cacheCleared = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/cache/recreate`));
    await cacheSection.getByRole("button", { name: "Clear cache", exact: true }).click();
    const cacheResponse = await cacheCleared;
    const cacheOutcome = await cacheResponse.json();
    if (cacheResponse.status() !== 200 || cacheOutcome.code !== "project.cache_cleared" || cacheOutcome.data !== null) throw new Error(`cache clear returned ${JSON.stringify(cacheOutcome)}`);
    const projectAfterCacheClear = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id), id);
    if (projectAfterCacheClear.gitCacheBytes !== 0 || projectAfterCacheClear.lfsCacheBytes !== 0) throw new Error(`cache sizes were not cleared: ${JSON.stringify(projectAfterCacheClear)}`);
    const refsAfterCacheClear = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data, id);
    if (!refsAfterCacheClear.some(ref => ref.kind === "branch" && ref.name === "main")) throw new Error("cache clearing removed the cached-ref metadata");
    console.log("combined-binary project outcome browser slice passed: project create, cached refs, refresh, and cache clearing");
  } else if (process.env.REPOSITORY_OUTCOMES_ONLY === "true") {
    milestone("exercise repository-key and SSH-host SDK operations against the combined binary");
    await page.goto(`https://${appListen}/dashboard/keys`, { waitUntil: "domcontentloaded" });
    await page.getByLabel("Key name").fill("Repository outcomes browser key");
    await page.getByRole("button", { name: "Create Ed25519 key" }).click();
    await page.getByText("Repository outcomes browser key", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Remove key" }).click();
    await page.getByText("No repository keys yet.", { exact: true }).waitFor();
    await page.goto(`https://${appListen}/dashboard/ssh-hosts`, { waitUntil: "domcontentloaded" });
    await page.getByRole("heading", { name: "SSH host keys" }).waitFor();
    await page.getByText("No SSH host warnings.", { exact: true }).waitFor();
    console.log("combined-binary repository outcome browser slice passed: key list/create/delete, SSH-host list, and LFS settings load/save");
  } else {
  const cacheRecreationPath = `/api/projects/${id}/cache/recreate`;
  const unauthenticatedCacheRecreation = await httpsRequestStatus("127.0.0.1", 18440, cacheRecreationPath, "POST", JSON.stringify({ confirm: true }), { Origin: `https://${appListen}`, "Content-Type": "application/json" });
  if (unauthenticatedCacheRecreation !== 401) throw new Error(`unauthenticated cache recreation returned ${unauthenticatedCacheRecreation}`);
  const crossOriginCacheRecreation = await httpsRequestStatus("127.0.0.1", 18440, cacheRecreationPath, "POST", JSON.stringify({ confirm: true }), { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" });
  if (crossOriginCacheRecreation !== 403) throw new Error(`cross-origin cache recreation returned ${crossOriginCacheRecreation}`);
  const giteaSettingsPath = `/api/projects/${id}/gitea-webhook`;
  for (const method of ["GET", "POST"]) {
    const unauthenticatedStatus = await httpsRequestStatus("127.0.0.1", 18440, giteaSettingsPath, method, method === "POST" ? JSON.stringify({ rotate: true }) : "", { Origin: `https://${appListen}`, "Content-Type": "application/json" });
    if (unauthenticatedStatus !== 401) throw new Error(`unauthenticated Gitea setup ${method} returned ${unauthenticatedStatus}`);
  }
  const crossOriginGitea = await httpsRequestStatus("127.0.0.1", 18440, giteaSettingsPath, "POST", JSON.stringify({ rotate: true }), { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" });
  if (crossOriginGitea !== 403) throw new Error(`cross-origin Gitea rotation returned ${crossOriginGitea}`);
  milestone("configure and rotate Gitea webhook");
  const giteaWebhook = page.getByRole("region", { name: "Gitea webhook", exact: true });
  await giteaWebhook.getByRole("button", { name: "Set up webhook", exact: true }).click();
  await giteaWebhook.getByRole("heading", { name: "Add this webhook in Gitea" }).waitFor();
  const webhookSecret = () => giteaWebhook.locator("dl > div").filter({ has: page.locator("dt", { hasText: /^Secret$/ }) }).locator("code").innerText();
  const firstGiteaSecret = await webhookSecret();
  if (!firstGiteaSecret) throw new Error("Gitea setup did not show a signing secret");
  await giteaWebhook.getByText(`https://${appListen}/webhooks/gitea/${id}`, { exact: true }).waitFor();
  const giteaStatus = await page.evaluate(async path => await (await fetch(path)).json(), giteaSettingsPath);
  if (!giteaStatus.configured || "secret" in giteaStatus) throw new Error("Gitea settings did not hide its configured secret");
  async function sendGitea(secret, delivery) {
    const body = JSON.stringify({ ref: "refs/heads/browser-unmatched", after: "a".repeat(40), deleted: false });
    return httpsRequestStatus("127.0.0.1", 18440, `/webhooks/gitea/${id}`, "POST", body, { "Content-Type": "application/json", "X-Gitea-Delivery": delivery, "X-Gitea-Event": "push", "X-Gitea-Signature": createHmac("sha256", secret).update(body).digest("hex") });
  }
  if (await sendGitea(firstGiteaSecret, "browser-gitea-initial") !== 204) throw new Error("initial Gitea secret did not authenticate a push");
  page.once("dialog", dialog => dialog.accept());
  await giteaWebhook.getByRole("button", { name: "Rotate secret", exact: true }).click();
  await waitFor(async () => (await webhookSecret()) !== firstGiteaSecret, "rotated Gitea secret");
  const rotatedGiteaSecret = await webhookSecret();
  if (!rotatedGiteaSecret) throw new Error("Gitea rotation did not show a new secret");
  if (await sendGitea(firstGiteaSecret, "browser-gitea-old") !== 401) throw new Error("old Gitea secret still authenticated after rotation");
  if (await sendGitea(rotatedGiteaSecret, "browser-gitea-new") !== 204) throw new Error("rotated Gitea secret did not authenticate a push");
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Fixture site", exact: true }).waitFor();
  await giteaWebhook.getByRole("button", { name: "Rotate secret", exact: true }).waitFor();
  if ((await page.locator("body").innerText()).includes(rotatedGiteaSecret)) throw new Error("Gitea secret remained after dashboard reload");
  milestone("create branch deployment");
  let releaseJobHistory;
  const jobHistoryRequest = new Promise(resolveRequest => {
    page.route("**/api/deployments/*/jobs", async route => {
      if (route.request().method() === "GET" && releaseJobHistory) {
        const release = releaseJobHistory;
        releaseJobHistory = null;
        resolveRequest(route);
        await release;
        return;
      }
      await route.continue();
    });
  });
  let finishJobHistory;
  releaseJobHistory = new Promise(resolveRelease => { finishJobHistory = resolveRelease; });
  await page.getByLabel("Deployment name").fill("Production site");
  const publicationRootResponse = await page.evaluate(async () => {
    const response = await fetch("/api/deployments/root");
    return { status: response.status, body: await response.json() };
  });
  if (publicationRootResponse.status !== 200 || publicationRootResponse.body.code !== "deployment.root_loaded" || publicationRootResponse.body.data.publicationRoot !== join(dir, "state.sqlite.publications")) throw new Error(`unexpected publication root outcome: ${JSON.stringify(publicationRootResponse)}`);
  const publishedPath = join(publicationRootResponse.body.data.publicationRoot, "production-site");
  await page.getByLabel("Publication folder").fill("production-site");
  await page.getByText(`Full path: ${publishedPath}`, { exact: true }).waitFor();
  try { await page.locator('option[value="branch:feature/search"]').waitFor({ state: "attached" }); } catch {
    const options = await page.locator("form select").first().innerHTML();
    const cached = await page.evaluate(async projectID => (await (await fetch(`/api/projects/${projectID}/refs`)).json()).data, id);
    throw new Error(`deployment targets missing feature branch: ${options}; cached refs: ${JSON.stringify(cached)}`);
  }
  await page.locator("form").locator("select").selectOption("branch:feature/search");
  const deploymentResponse = page.waitForResponse(response => response.url().endsWith("/api/deployments") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create deployment" }).click();
  const deploymentCreated = await deploymentResponse;
  if (deploymentCreated.status() !== 201) throw new Error(`deployment creation returned ${deploymentCreated.status()}: ${await deploymentCreated.text()}`);
  if (deploymentCreated.request().postDataJSON().publicationPath !== "production-site") throw new Error("dashboard did not submit the chosen publication folder");
  const createdDeployment = await deploymentCreated.json();
  if (createdDeployment.code !== "deployment.created" || createdDeployment.data.status !== "queued" || !createdDeployment.data.jobId) throw new Error(`deployment creation did not report queued acceptance: ${JSON.stringify(createdDeployment)}`);
  const deploymentID = createdDeployment.data.id;
  if (createdDeployment.data.publicationPath !== publishedPath) throw new Error(`creation returned a different publication path: ${JSON.stringify(createdDeployment)}`);
  await page.waitForURL(`**/dashboard/projects/${id}/deployments/${deploymentID}`);
  await page.getByRole("heading", { name: "Production site", exact: true }).waitFor();
  await page.getByText(`Publication path: ${publishedPath}`, { exact: true }).waitFor();
  const listedDeployment = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).data.find(item => item.id === id), deploymentID);
  if (listedDeployment?.publicationPath !== publishedPath) throw new Error(`deployment list returned a different publication path: ${JSON.stringify(listedDeployment)}`);
  if (await httpsRequestStatus("127.0.0.1", 18440, "/api/deployments", "GET") !== 401) throw new Error("deployment list was accessible without a session");
  if (await httpsRequestStatus("127.0.0.1", 18440, `/api/deployments/${deploymentID}/jobs`, "GET") !== 401) throw new Error("deployment history was accessible without a session");
  if (await httpsRequestStatus("127.0.0.1", 18440, `/api/deployments/${deploymentID}/deploy`, "POST", "{}", { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" }) !== 403) throw new Error("cross-origin deployment action was accepted");
  const delayedHistoryRoute = await jobHistoryRequest;
  const productionHistory = page.getByRole("region", { name: "Job history for Production site" });
  await productionHistory.getByText("Loading history…", { exact: true }).waitFor();
  if (await productionHistory.getByText("No deployment history yet.", { exact: true }).count()) throw new Error("pending deployment history was shown as empty");
  finishJobHistory();
  await delayedHistoryRoute.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ code: "job.listed", message: "Jobs loaded.", data: [] }) });
  await productionHistory.getByText("No deployment history yet.", { exact: true }).waitFor();
  if (await productionHistory.getByText("No deployment jobs recorded yet.", { exact: true }).count()) throw new Error("outdated deployment history empty message remained visible");
  let failHistoryRoute;
  const failedHistoryRequest = new Promise(resolveRequest => { failHistoryRoute = resolveRequest; });
  await page.route(`**/api/deployments/${deploymentID}/jobs`, async route => {
    await page.unroute(`**/api/deployments/${deploymentID}/jobs`);
    failHistoryRoute(route);
    await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE JOB HISTORY ERROR" });
  });
  await failedHistoryRequest;
  await productionHistory.getByRole("alert").filter({ hasText: "Could not load deployment history" }).waitFor();
  if (await productionHistory.getByText("No deployment history yet.", { exact: true }).count()) throw new Error("failed deployment history was shown as empty");
  if ((await productionHistory.innerText()).includes("SENSITIVE JOB HISTORY ERROR")) throw new Error("deployment history exposed an HTTP response body");
  try {
  await waitFor(async () => {
      try {
        return (await readFile(join(publishedPath, "index.html"), "utf8")) === "<!doctype html><title>feature search</title>\n" && (await readFile(join(publishedPath, "asset.dat"), "utf8")) === "fixture hydrated LFS asset\n";
      } catch { return false; }
    }, "published deployment files");
  } catch (error) {
    if (error === focusedOutcomesComplete) throw error;
    const state = await page.evaluate(async id => ({ deployment: await (await fetch("/api/deployments")).text(), jobs: await (await fetch(`/api/deployments/${id}/jobs`)).text() }), deploymentID);
    throw new Error(`${error.message}; state: ${JSON.stringify(state)}`);
  }
  const completedOutcome = await page.evaluate(async id => {
    const [deployments, jobs] = await Promise.all([
      fetch("/api/deployments").then(response => response.json()),
      fetch(`/api/deployments/${id}/jobs`).then(response => response.json())
    ]);
    return { deployment: deployments.data.find(item => item.id === id), job: jobs.data[0] };
  }, deploymentID);
  if (completedOutcome.deployment.liveCommit !== completedOutcome.job.commit || completedOutcome.job.status !== "succeeded") {
    throw new Error(`queue acceptance was confused with completed publication: ${JSON.stringify(completedOutcome)}`);
  }
  const safeFailure = await page.evaluate(async id => {
    const response = await fetch(`/api/deployments/${id}/target`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ targetKind: "branch", targetName: "not-cached" })
    });
    return { status: response.status, body: await response.json() };
  }, deploymentID);
  if (safeFailure.status !== 400 || safeFailure.body.code !== "git.target_unavailable" || safeFailure.body.data !== null ||
      /SENSITIVE|private|filesystem|repository path/i.test(safeFailure.body.message)) {
    throw new Error(`target failure was not a safe semantic outcome: ${JSON.stringify(safeFailure)}`);
  }
  if (process.env.DEPLOYMENT_OUTCOMES_ONLY) {
    console.log("combined-binary deployment acceptance, completion, and safe failure assertions passed");
    throw focusedOutcomesComplete;
  }
  await page.getByText("Live", { exact: true }).waitFor();
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Production site", exact: true }).waitFor();
  await page.getByRole("link", { name: "Fixture site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}`);
  await page.getByText(`Publication path: ${publishedPath}`, { exact: true }).waitFor();
  await page.getByRole("link", { name: "Production site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}/deployments/${deploymentID}`);
  await page.goto(`https://${appListen}/dashboard/projects/${id}/deployments/missing-deployment`, { waitUntil: "domcontentloaded" });
  await page.getByText(/Deployment not found/i).waitFor();
  await page.goto(`https://${appListen}/dashboard/projects/${ordinaryProject.id}/deployments/${deploymentID}`, { waitUntil: "domcontentloaded" });
  await page.getByText(/Deployment not found/i).waitFor();
  if (await page.getByRole("button", { name: "Deploy", exact: true }).count()) throw new Error("another project's deployment exposed its actions");
  await page.goto(`https://${appListen}/dashboard/projects/${id}/deployments/${deploymentID}`, { waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Production site", exact: true }).waitFor();
  await page.getByRole("link", { name: "Fixture site", exact: true }).click();
  await page.getByRole("link", { name: "Production site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}/deployments/${deploymentID}`);
  const firstDirectoryIdentity = (await stat(publishedPath)).ino;
  milestone("steady deployment history during background refreshes");
  const realJobHistory = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
  if (!realJobHistory.length) throw new Error("deployment history fixture has no recorded jobs");
  const historyJob = (status, id) => ({ ...realJobHistory[0], id, status, updatedAt: new Date().toISOString() });
  const historyResponses = [
    [historyJob("queued", "history-stable-a"), historyJob("succeeded", "history-stable-b")],
    [historyJob("running", "history-stable-a"), historyJob("succeeded", "history-stable-b")],
    [historyJob("succeeded", "history-stable-a"), historyJob("succeeded", "history-stable-b")]
  ];
  let historyPollCount = 0;
  let heldHistoryPolls = 0;
  const releaseHistoryPolls = [];
  await page.route(`**/api/deployments/${deploymentID}/jobs`, async route => {
    if (route.request().method() !== "GET") return route.continue();
    const poll = ++historyPollCount;
    if (poll === 2 || poll === 3) {
      heldHistoryPolls++;
      await new Promise(resolveHistory => releaseHistoryPolls.push(resolveHistory));
    }
    if (poll === 4) {
      await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE POLLING FAILURE" });
      return;
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(historyResponses[Math.min(poll - 1, 2)]) });
  });
  const stableHistory = page.getByRole("region", { name: "Job history for Production site" });
  await stableHistory.getByText("Queued", { exact: true }).waitFor();
  if (await stableHistory.locator("ul > li").count() !== 2) throw new Error("initial job history list was not rendered");
  for (const [poll, status] of [[2, "Queued"], [3, "Running"]]) {
    await waitFor(() => heldHistoryPolls >= poll - 1, `delayed history refresh ${poll}`);
    await stableHistory.getByText(status, { exact: true }).waitFor();
    if (await stableHistory.getByText("Loading history…", { exact: true }).count()) throw new Error("background polling replaced visible deployment history with a loading state");
    if (await stableHistory.locator("ul > li").count() !== 2) throw new Error("background polling changed the visible history list while a refresh was pending");
    releaseHistoryPolls[poll - 2]();
    await stableHistory.getByText(poll === 2 ? "Running" : "Succeeded", { exact: true }).first().waitFor();
  }
  await stableHistory.getByRole("alert").filter({ hasText: "Could not load deployment history" }).waitFor();
  await stableHistory.getByText("Succeeded", { exact: true }).first().waitFor();
  if (await stableHistory.locator("ul > li").count() !== 2) throw new Error("failed history refresh discarded the previously loaded jobs");
  if ((await stableHistory.innerText()).includes("SENSITIVE POLLING FAILURE")) throw new Error("history refresh exposed an HTTP response body");
  await waitFor(() => historyPollCount >= 5, "deployment history recovery poll");
  await waitFor(async () => !(await stableHistory.getByRole("alert").count()), "deployment history recovery");
  await stableHistory.getByText("Succeeded", { exact: true }).first().waitFor();
  if (await stableHistory.locator("ul > li").count() !== 2) throw new Error("successful history recovery did not retain the visible job list");
  await page.unroute(`**/api/deployments/${deploymentID}/jobs`);
  const mainCommit = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data.find(ref => ref.kind === "branch" && ref.name === "main").commit, id);
  const featureCommit = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data.find(ref => ref.kind === "branch" && ref.name === "feature/search").commit, id);
  milestone("open rollback history");
  await page.getByRole("button", { name: "Rollback", exact: true }).click();
  const history = page.getByRole("region", { name: "Rollback history for Production site" });
  await history.getByText("Page 1", { exact: true }).waitFor();
  for (const [commit, title] of [[mainCommit, "main"], [featureCommit, "feature search"]]) {
    milestone(`search rollback history: ${title}`);
    await history.getByLabel("Search commits").fill(commit);
    const searched = page.waitForResponse(response => response.url().includes(`/api/deployments/${deploymentID}/history?`));
    await history.getByRole("button", { name: "Search", exact: true }).click();
    if ((await searched).status() !== 200) throw new Error("rollback history search failed");
    milestone(`select rollback commit: ${title}`);
    await history.getByRole("button", { name: `Roll back to ${commit.slice(0, 12)}`, exact: true }).click();
    await history.getByRole("radio", { name: /Temporary rollback/ }).waitFor();
    // Cancellation must not publish; the next confirmation uses the full ID.
    await history.getByRole("button", { name: "Cancel", exact: true }).click();
    await history.getByRole("button", { name: `Roll back to ${commit.slice(0, 12)}`, exact: true }).click();
    if (title === "main") {
      const rollbackURL = `**/api/deployments/${deploymentID}/rollback`;
      for (const [label, body] of [["malformed JSON", "{RAW_ROLLBACK_ERROR"], ["null response", "null"], ["missing job ID", "{}"], ["non-string job ID", '{"jobId":42}'], ["blank job ID", '{"jobId":"   "}']]) {
        milestone(`reject rollback acceptance: ${label}`);
        await page.route(rollbackURL, route => route.fulfill({ status: 202, contentType: "application/json", body }));
        try {
          const invalidResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/rollback`));
          await history.getByRole("button", { name: "Confirm temporary rollback", exact: true }).click();
          if ((await invalidResponse).status() !== 202) throw new Error("invalid rollback acceptance fixture did not run");
          await history.getByRole("alert").filter({ hasText: "Could not confirm the rollback request. Check Latest deployment before retrying." }).waitFor();
          if (await history.getByRole("status").filter({ hasText: /Rollback to .* queued/ }).count()) throw new Error(`${label} incorrectly showed rollback queued`);
          if (await history.getByRole("button", { name: "Confirm temporary rollback", exact: true }).isDisabled()) throw new Error(`${label} left rollback controls disabled`);
          if ((await history.innerText()).includes("RAW_ROLLBACK_ERROR")) throw new Error("rollback exposed a malformed HTTP response body");
        } finally {
          await page.unroute(rollbackURL);
        }
      }
    }
    const rollbackResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/rollback`));
    milestone(`confirm rollback: ${title}`);
    await history.getByRole("button", { name: "Confirm temporary rollback", exact: true }).click();
    const accepted = await rollbackResponse;
    if (accepted.status() !== 202 || accepted.request().postDataJSON().commit !== commit) throw new Error("rollback did not use the full selected commit ID");
    // Headers alone do not prove that the browser completed this request.
    // Keep the response body/job assertion bounded to catch unread responses.
    const rollbackJob = await withDeadline(accepted.json(), "rollback acceptance response body");
    await waitFor(async () => {
      const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
      return jobs.find(job => job.id === rollbackJob.jobId)?.status === "succeeded";
    }, "recorded rollback completion");
    await waitFor(async () => (await readFile(join(publishedPath, "index.html"), "utf8")) === `<!doctype html><title>${title}</title>\n`, "dashboard rollback publication");
    if (title === "feature search" && (await readFile(join(publishedPath, "asset.dat"), "utf8")) !== "fixture hydrated LFS asset\n") throw new Error("rollback did not recreate LFS content");
    const state = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).find(item => item.id === id), deploymentID);
    if (state.targetKind !== "branch" || state.targetName !== "feature/search" || state.liveCommit !== commit) throw new Error("temporary rollback changed the branch target");
  }
  milestone("confirm permanent rollback behavior");
  await history.getByLabel("Search commits").fill("");
  const fullHistory = page.waitForResponse(response => response.url().includes(`/api/deployments/${deploymentID}/history?`));
  await history.getByRole("button", { name: "Search", exact: true }).click();
  if ((await fullHistory).status() !== 200) throw new Error("could not reload rollback history");
  await history.getByRole("button", { name: `Roll back to ${mainCommit.slice(0, 12)}`, exact: true }).click();
  await history.getByRole("radio", { name: /Permanent rollback/ }).check();
  await history.getByText(/The pause remains even if the rollback fails/).waitFor();
  const permanentResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/rollback`));
  await history.getByRole("button", { name: "Confirm permanent rollback", exact: true }).click();
  const permanent = await permanentResponse;
  const permanentRequest = permanent.request().postDataJSON();
  if (permanent.status() !== 202 || permanentRequest.commit !== mainCommit || permanentRequest.permanent !== true) {
    throw new Error("permanent rollback did not send the selected commit and pause choice");
  }
  const permanentJob = await withDeadline(permanent.json(), "permanent rollback acceptance response body");
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
    return jobs.find(job => job.id === permanentJob.jobId)?.status === "succeeded";
  }, "permanent rollback completion");
  const permanentlyPaused = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).find(item => item.id === id), deploymentID);
  if (!permanentlyPaused.automationPaused || permanentlyPaused.targetKind !== "branch" || permanentlyPaused.targetName !== "feature/search" || permanentlyPaused.liveCommit !== mainCommit) {
    throw new Error("permanent rollback changed its branch target or failed to pause automation");
  }
  milestone("resume after permanent rollback");
  const resumeResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/resume`));
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  const permanentResume = await resumeResponse;
  if (permanentResume.status() !== 202) throw new Error("could not resume after permanent rollback");
  const resumedJob = await permanentResume.json();
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
    return jobs.find(job => job.id === resumedJob.jobId)?.status === "succeeded";
  }, "deployment after permanent rollback resume");
  milestone("change branch target and deploy no-op");
  await page.getByRole("button", { name: "Rollback", exact: true }).click();
  await page.getByLabel("Target for Production site").selectOption("branch:main");
  const targetUpdate = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/target`));
  await page.getByRole("button", { name: "Change target" }).click();
  if ((await targetUpdate).status() !== 202) throw new Error("changing the deployment branch did not queue publication");
  await waitFor(async () => {
    try { return (await readFile(join(publishedPath, "index.html"), "utf8")) === "<!doctype html><title>main</title>\n"; } catch { return false; }
  }, "replacement publication files");
  if ((await stat(publishedPath)).ino === firstDirectoryIdentity) throw new Error("replacement did not switch the live directory");
  const productionCard = page.getByRole("main");
  await page.getByRole("button", { name: "Deploy", exact: true }).click();
  await waitForNoopHistory(page, deploymentID, productionCard);

  await productionCard.getByText("Automatic deployment enabled", { exact: true }).waitFor();
  milestone("pause failure: intercept request");
  const pauseFailureGate = new Promise(resolveGate => { releasePauseFailure = resolveGate; });
  const pauseURL = `**/api/deployments/${deploymentID}/pause`;
  await page.route(pauseURL, async route => {
    milestone("pause failure: request intercepted");
    await pauseFailureGate;
    await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE PAUSE ERROR" });
  });
  const rejectedPause = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/pause`));
  milestone("pause failure: click Pause");
  await productionCard.getByRole("button", { name: "Pause", exact: true }).click();
  milestone("pause failure: assert disabled and release request");
  if (!(await productionCard.getByRole("button", { name: "Pause", exact: true }).isDisabled())) throw new Error("Pause stayed enabled during its request");
  releasePauseFailure();
  if ((await rejectedPause).status() !== 500) throw new Error("Pause failure fixture did not run");
  await page.getByRole("alert").filter({ hasText: "The request could not be completed. Try again shortly." }).waitFor();
  await productionCard.getByText("Automatic deployment enabled", { exact: true }).waitFor();
  if ((await page.locator("body").innerText()).includes("SENSITIVE PAUSE ERROR")) throw new Error("Pause exposed an HTTP response body");
  await page.unroute(pauseURL);
  milestone("pause, reload, manual deploy, resume");
  const pausedResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/pause`));
  await productionCard.getByRole("button", { name: "Pause", exact: true }).click();
  const paused = await pausedResponse;
  if (!paused.ok() || (await paused.json()).automationPaused !== true) throw new Error("Pause did not persist automation state");
  await productionCard.getByText("Automatic deployment paused", { exact: true }).waitFor();
  await page.reload();
  await page.getByRole("heading", { name: "Production site", exact: true }).waitFor();
  await productionCard.getByText("Automatic deployment paused", { exact: true }).waitFor();
  const manualWhilePaused = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/deploy`));
  await productionCard.getByRole("button", { name: "Deploy", exact: true }).click();
  if ((await manualWhilePaused).status() !== 202) throw new Error("manual Deploy was blocked while paused");
  await waitForNoopHistory(page, deploymentID, productionCard);
  const stillPaused = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).find(item => item.id === id).automationPaused, deploymentID);
  if (!stillPaused) throw new Error("manual Deploy cleared automation pause");
  const resumedResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/resume`));
  await productionCard.getByRole("button", { name: "Resume", exact: true }).click();
  const resumed = await resumedResponse;
  const resumeBody = await resumed.json();
  if (resumed.status() !== 202 || resumeBody.automationPaused !== false || !resumeBody.jobId || resumeBody.status !== "queued") throw new Error("Resume did not enable automation and queue deployment");
  await productionCard.getByText("Automatic deployment enabled", { exact: true }).waitFor();

  await page.getByRole("link", { name: "Fixture site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}`);
  const initialTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id).lastFetched, id);
  const search = page.getByLabel("Search branches and tags");
  await search.fill("feature/search");
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  if (await page.getByText("v1.0", { exact: true }).count()) throw new Error("branch search left a non-matching tag visible");
  await search.fill("v1.0");
  await page.getByText("v1.0", { exact: true }).waitFor();
  if (await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).count()) throw new Error("tag search left a non-matching branch visible");
  await search.fill("");

  milestone("create tag deployment and hard re-deploy");
  await page.getByLabel("Deployment name").fill("Tag release");
  await page.getByLabel("Publication folder").fill("tag-release");
  await page.locator("form select").selectOption("tag:v1.0");
  const tagDeploymentResponse = page.waitForResponse(response => response.url().endsWith("/api/deployments") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Create deployment" }).click();
  const tagCreated = await tagDeploymentResponse;
  if (tagCreated.status() !== 201) throw new Error(`tag deployment creation returned ${tagCreated.status()}: ${await tagCreated.text()}`);
  const tagDeployment = await tagCreated.json();
  await page.waitForURL(`**/dashboard/projects/${id}/deployments/${tagDeployment.id}`);
  const tagPath = join(publicationRootResponse.body.publicationRoot, "tag-release");
  if (tagDeployment.publicationPath !== tagPath) throw new Error(`tag deployment returned a different publication path: ${JSON.stringify(tagDeployment)}`);
  await waitFor(async () => {
    try { return (await readFile(join(tagPath, "index.html"), "utf8")) === "<!doctype html><title>feature search</title>\n"; } catch { return false; }
  }, "tag-target publication from the dashboard");
  const tagCard = page.getByRole("main");
  if (await tagCard.getByRole("button", { name: /^(Pause|Resume)$/ }).count()) throw new Error("tag deployment showed branch-only automation controls");
  const tagIdentity = (await stat(tagPath)).ino;
  const hardRedeployResponse = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${tagDeployment.id}/hard-redeploy`));
  await tagCard.getByRole("button", { name: "Hard re-deploy", exact: true }).click();
  if ((await hardRedeployResponse).status() !== 202) throw new Error("dashboard Hard re-deploy was not queued");
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), tagDeployment.id);
    return jobs[0]?.status === "succeeded" && (await stat(tagPath)).ino !== tagIdentity;
  }, "dashboard Hard re-deploy to recreate the tag publication");

  await page.getByRole("link", { name: "Fixture site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}`);
  milestone("refresh success and failure retention");
  const refreshButton = page.getByRole("button", { name: "Refresh branches and tags", exact: true });
  const refreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await refreshButton.click();
  const refreshed = await refreshResponse;
  if (refreshed.status() !== 200 || (await refreshed.json()).code !== "project.refreshed") throw new Error(`successful refresh did not return project.refreshed (${refreshed.status()})`);
  await page.locator("span.font-medium").filter({ hasText: /^feature\/search$/ }).waitFor();
  const refreshedTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id).lastFetched, id);
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
  const timestampAfterReloadFailure = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id).lastFetched, id);
  if (!timestampAfterReloadFailure || timestampAfterReloadFailure === refreshedTimestamp) throw new Error("server did not record the successful refresh before the failed list reload");

  const failFixture = await httpsRequestStatus("127.0.0.1", 18441, "/__fixture/fail", "POST");
  if (failFixture !== 204) throw new Error(`fixture fail control returned ${failFixture}`);
  const cachedRefs = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data, id);
  const failedRefreshResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/refresh`));
  await page.getByRole("button", { name: "Refresh branches and tags", exact: true }).click();
  const failedRefresh = await failedRefreshResponse;
  if (failedRefresh.status() !== 502 || (await failedRefresh.json()).code !== "operation.upstream_failed") throw new Error(`failed refresh did not return an actionable upstream outcome (${failedRefresh.status()})`);
  await page.getByText(/Your current branches and tags are still shown/).waitFor();
  const retainedRefs = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data, id);
  if (JSON.stringify(retainedRefs) !== JSON.stringify(cachedRefs)) throw new Error("failed refresh changed the cached refs");
  const failedTimestamp = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id).lastFetched, id);
  if (failedTimestamp !== timestampAfterReloadFailure) throw new Error("failed refresh changed last successful fetch time");

  milestone("create SSH key, approve host, create SSH project");
  await page.getByRole("link", { name: "Repository keys", exact: true }).click();
  await page.waitForURL("**/dashboard/keys");
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Repository keys" }).waitFor();
  if (await page.getByRole("heading", { name: "Add a repository" }).count()) throw new Error("key management remains on the project list");
  await page.getByLabel("Key name").fill("SSH browser key");
  await page.getByRole("button", { name: "Create Ed25519 key" }).click();
  await page.getByText("SSH browser key", { exact: true }).waitFor();
  const generatedKey = page.locator("li").filter({ hasText: "SSH browser key" });
  if (!(await generatedKey.locator("code").innerText()).startsWith("ssh-ed25519 ") || !(await generatedKey.getByText(/SHA256:/).count())) throw new Error("public key or fingerprint is missing from the key page");
  const sshKeyId = await page.evaluate(async () => (await (await fetch("/api/repository-keys")).json()).data.find(key => key.name === "SSH browser key").id);
  const importedPrivateKey = generateKeyPairSync("ed25519").privateKey.export({ format: "pem", type: "pkcs8" }).toString();
  await page.getByLabel("Name for imported key").fill("Imported browser key");
  await page.getByLabel("Private key").fill(importedPrivateKey);
  await page.getByRole("button", { name: "Import private key" }).click();
  await page.getByText("Imported browser key", { exact: true }).waitFor();
  if (await page.getByLabel("Private key").inputValue()) throw new Error("private key input was not cleared after import");
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByText("Imported browser key", { exact: true }).waitFor();
  if ((await page.locator("body").innerText()).includes(importedPrivateKey) || (await page.content()).includes(importedPrivateKey)) throw new Error("private key material was displayed after import");
  const importedKey = page.locator("li").filter({ hasText: "Imported browser key" });
  await importedKey.getByRole("button", { name: "Remove key" }).click();
  await page.getByText("Imported browser key", { exact: true }).waitFor({ state: "detached" });
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.waitForURL("**/dashboard/projects");
  await page.getByLabel("Project name").fill("SSH fixture site");
  await page.getByLabel("Repository URL").fill("ssh://git@127.0.0.1:18442/repo.git");
  await page.getByLabel("SSH key").selectOption(sshKeyId);
  const blockedCreateResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const blockedCreate = await blockedCreateResponse;
  if (blockedCreate.status() !== 409) throw new Error(`unknown SSH host was not blocked (${blockedCreate.status()})`);
  await page.getByRole("link", { name: "SSH hosts", exact: true }).click();
  await page.waitForURL("**/dashboard/ssh-hosts");
  await page.reload({ waitUntil: "domcontentloaded" });
  const hostTrust = page.getByRole("main").filter({ has: page.getByRole("heading", { name: "SSH host keys" }) });
  await hostTrust.getByText(/SHA256:/).waitFor();
  if (!(await hostTrust.getByText(/Verify this fingerprint independently before approving it/).count())) throw new Error("host approval lacks independent verification warning");
  if (!(await hostTrust.getByText(/127\.0\.0\.1:18442/).count())) throw new Error("host warning lost host and port");
  page.once("dialog", dialog => dialog.dismiss());
  await page.getByRole("button", { name: "Approve verified host key" }).click();
  await hostTrust.getByText(/SHA256:/).waitFor();
  page.once("dialog", dialog => dialog.accept());
  await page.getByRole("button", { name: "Approve verified host key" }).click();
  await hostTrust.getByText("No SSH host warnings.").waitFor();
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.waitForURL("**/dashboard/projects");
  await page.getByLabel("Project name").fill("SSH fixture site");
  await page.getByLabel("Repository URL").fill("ssh://git@127.0.0.1:18442/repo.git");
  await page.getByLabel("SSH key").selectOption(sshKeyId);
  const sshCreateResponse = page.waitForResponse(response => response.url().endsWith("/api/projects") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Add repository" }).click();
  const sshCreated = await sshCreateResponse;
  if (sshCreated.status() !== 201) throw new Error(`approved SSH project creation returned ${sshCreated.status()}: ${await sshCreated.text()}`);
  const sshCreateOutcome = await sshCreated.json();
  if (sshCreateOutcome.code !== "project.created") throw new Error(`SSH project creation returned ${JSON.stringify(sshCreateOutcome)}`);
  const sshProject = sshCreateOutcome.data;
  await page.waitForURL(`**/dashboard/projects/${sshProject.id}`);
  const sshRefs = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/refs`)).json()).data, sshProject.id);
  if (!sshRefs.some(ref => ref.kind === "branch" && ref.name === "main")) throw new Error("approved SSH project did not cache its branch");
  await page.getByRole("link", { name: "Repository keys", exact: true }).click();
  await page.waitForURL("**/dashboard/keys");
  const assignedKey = page.locator("li").filter({ hasText: "SSH browser key" });
  await assignedKey.getByRole("button", { name: "Remove key" }).click();
  await page.getByText("This key is assigned to a project.").waitFor();
  await page.getByLabel("Key name").fill("Replacement SSH key");
  await page.getByRole("button", { name: "Create Ed25519 key" }).click();
  await page.getByText("Replacement SSH key", { exact: true }).waitFor();
  const replacementKeyId = await page.evaluate(async () => (await (await fetch("/api/repository-keys")).json()).data.find(key => key.name === "Replacement SSH key").id);
  await page.goto(`https://${appListen}/dashboard/projects/${sshProject.id}`, { waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "SSH fixture site", exact: true }).waitFor();
  await page.getByLabel("SSH key").selectOption(replacementKeyId);
  const assignKeyResponse = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/key`));
  await page.getByRole("button", { name: "Change SSH key" }).click();
  const assignedKeyOutcome = await assignKeyResponse;
  if (assignedKeyOutcome.status() !== 200 || (await assignedKeyOutcome.json()).code !== "project.key_assigned") throw new Error(`SSH key assignment returned an unexpected outcome (${assignedKeyOutcome.status()})`);
  await waitFor(async () => (await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id)?.keyId, sshProject.id)) === replacementKeyId, "project key assignment");
  await page.getByLabel("SSH key").selectOption(sshKeyId);
  await page.getByRole("button", { name: "Change SSH key" }).click();
  await waitFor(async () => (await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id)?.keyId, sshProject.id)) === sshKeyId, "restored project key assignment");
  milestone("configure private LFS and publish SSH deployment");
  let oldProjectHistoryPolls = 0;
  await page.route(`**/api/deployments/${deploymentID}/jobs`, async route => {
    if (route.request().method() === "GET") oldProjectHistoryPolls++;
    await route.continue();
  });
  await page.getByRole("heading", { name: "SSH fixture site", exact: true }).waitFor();
  await page.getByLabel("LFS server URL").waitFor();
  const sshEmptyLfs = await page.evaluate(async id => (await (await fetch(`/api/projects/${id}/lfs`)).json()).data, sshProject.id);
  if (!Array.isArray(sshEmptyLfs.settings.objectOrigins) || sshEmptyLfs.settings.objectOrigins.length !== 0) throw new Error(`ordinary SSH repository LFS settings had an inconsistent shape: ${JSON.stringify(sshEmptyLfs)}`);
  if (await page.getByLabel("LFS server URL").inputValue() !== "" || await page.getByLabel("Object download origins").inputValue() !== "") throw new Error("unconfigured project displayed unexpected LFS destinations");
  await page.getByText("No deployments for this project yet.", { exact: true }).waitFor();
  const oldProjectPollBaseline = oldProjectHistoryPolls;
  await new Promise(resolveWait => setTimeout(resolveWait, 2200));
  if (oldProjectHistoryPolls !== oldProjectPollBaseline) throw new Error("deployment history polling continued after switching projects");
  await page.unroute(`**/api/deployments/${deploymentID}/jobs`);
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
  const sshPublishedPath = join(publicationRootResponse.body.publicationRoot, sshDeployment.body.id);
  if (sshDeployment.body.publicationPath !== sshPublishedPath) throw new Error(`legacy deployment did not retain its ID-based publication path: ${JSON.stringify(sshDeployment)}`);
  await waitFor(async () => {
    try {
      return (await readFile(join(sshPublishedPath, "index.html"), "utf8")) === "SSH fixture site\n" && (await readFile(join(sshPublishedPath, "asset.dat"), "utf8")) === "fixture hydrated LFS asset\n";
    } catch { return false; }
  }, "SSH cached-object publication");
  const privateJobLog = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).text(), sshDeployment.body.id);
  for (const secret of ["ssh-fixture-private-lfs", "private-fixture-secret", "Bearer"]) {
    if (privateJobLog.includes(secret) || appOutput.includes(secret)) throw new Error(`private LFS secret reached an application or job log (${secret})`);
  }
  const listedLegacyDeployment = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).find(item => item.id === id), sshDeployment.body.id);
  if (listedLegacyDeployment?.publicationPath !== sshPublishedPath) throw new Error(`legacy deployment list path changed: ${JSON.stringify(listedLegacyDeployment)}`);
  milestone("deployment refs error recovers without hiding other errors");
  let failedDeploymentRefs = false;
  await page.route(`**/api/projects/${sshProject.id}/refs`, async route => {
    if (!failedDeploymentRefs && route.request().frame().url().includes(`/deployments/${sshDeployment.body.id}`)) {
      failedDeploymentRefs = true;
      await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE_REFS_FAILURE" });
    } else {
      await route.continue();
    }
  });
  await page.getByRole("link", { name: "SSH published site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${sshProject.id}/deployments/${sshDeployment.body.id}`);
  const commitCard = page.getByRole("main");
  await commitCard.waitFor();
  await commitCard.getByRole("alert").filter({ hasText: "The request could not be completed. Try again shortly." }).waitFor();
  await commitCard.getByRole("alert").waitFor({ state: "detached", timeout: 10000 });
  if (!failedDeploymentRefs || (await commitCard.innerText()).includes("SENSITIVE_REFS_FAILURE")) throw new Error("deployment refs failure did not recover safely");
  await page.unroute(`**/api/projects/${sshProject.id}/refs`);
  await commitCard.getByText(`Target commit: ${sshCommit}`, { exact: true }).waitFor();
  await commitCard.getByText(`Live commit: ${sshCommit}`, { exact: true }).waitFor();
  await commitCard.getByText(`Publication path: ${sshPublishedPath}`, { exact: true }).waitFor();
  milestone("move legacy publication into a readable folder");
  const movePath = `/api/deployments/${sshDeployment.body.id}/move`;
  const movedPath = join(publicationRootResponse.body.publicationRoot, "ssh-readable-site");
  const intermediatePath = join(publicationRootResponse.body.publicationRoot, "ssh-intermediate-site");
  const occupiedPath = join(publicationRootResponse.body.publicationRoot, "occupied-move-site");
  const originalIdentity = (await stat(sshPublishedPath)).ino;
  const originalContent = await readFile(join(sshPublishedPath, "index.html"), "utf8");
  const originalAsset = await readFile(join(sshPublishedPath, "asset.dat"), "utf8");
  if (await httpsRequestStatus("127.0.0.1", 18440, movePath, "POST", JSON.stringify({ publicationPath: "ssh-readable-site", confirm: true }), { Origin: `https://${appListen}`, "Content-Type": "application/json" }) !== 401) throw new Error("unauthenticated publication move was accepted");
  if (await httpsRequestStatus("127.0.0.1", 18440, movePath, "POST", JSON.stringify({ publicationPath: "ssh-readable-site", confirm: true }), { Cookie: `__Host-deploy-manager-session=${sessionCookie}`, Origin: "https://attacker.example", "Content-Type": "application/json" }) !== 403) throw new Error("cross-origin publication move was accepted");
  await mkdir(occupiedPath);
  await writeFile(join(occupiedPath, "keep.txt"), "not owned by deployment\n");
  const occupiedMove = await page.evaluate(async ([path, originalPath]) => {
    const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ publicationPath: "occupied-move-site", expectedPublicationPath: originalPath, confirm: true }) });
    return response.status;
  }, [movePath, sshPublishedPath]);
  if (occupiedMove !== 409) throw new Error(`moving into an occupied folder returned ${occupiedMove}, expected 409`);
  if ((await readFile(join(occupiedPath, "keep.txt"), "utf8")) !== "not owned by deployment\n" || (await stat(sshPublishedPath)).ino !== originalIdentity) throw new Error("failed move changed the existing publication or occupied destination");
  await commitCard.getByRole("button", { name: /Move publication/i }).click();
  await commitCard.getByLabel(/Publication folder/i).fill("ssh-readable-site");
  const moveReview = commitCard.getByRole("dialog", { name: /move/i });
  await commitCard.getByRole("button", { name: /Review move/i }).click();
  await moveReview.waitFor();
  const warning = await moveReview.innerText();
  if (!warning.includes(sshPublishedPath) || !warning.includes(movedPath)) throw new Error(`move warning did not identify both old and new paths: ${warning}`);
  await moveReview.getByRole("button", { name: /Cancel/i }).click();
  if ((await stat(sshPublishedPath)).ino !== originalIdentity) throw new Error("cancelling publication move changed the live directory");
  await commitCard.getByRole("button", { name: /Review move/i }).click();
  // Another administrator moves the site while this confirmation still
  // displays the old root. The stale confirmation must not move it again.
  const concurrentMove = await page.evaluate(async ([path, originalPath]) => {
    const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ publicationPath: "ssh-intermediate-site", expectedPublicationPath: originalPath, confirm: true }) });
    return response.status;
  }, [movePath, sshPublishedPath]);
  if (concurrentMove !== 200) throw new Error(`concurrent move failed (${concurrentMove})`);
  const staleResponse = page.waitForResponse(response => response.url().endsWith(movePath) && response.request().method() === "POST");
  await moveReview.getByRole("button", { name: /Confirm move/i }).click();
  if ((await staleResponse).status() !== 409 || (await stat(intermediatePath)).ino !== originalIdentity) throw new Error("stale confirmation moved the publication again");
  try { await stat(movedPath); throw new Error("stale confirmation created its proposed destination"); } catch (error) { if (error.code !== "ENOENT") throw error; }
  await page.reload();
  await commitCard.getByText(`Publication path: ${intermediatePath}`, { exact: true }).waitFor();
  await commitCard.getByRole("button", { name: /Move publication/i }).click();
  await commitCard.getByLabel(/Publication folder/i).fill("ssh-readable-site");
  await commitCard.getByRole("button", { name: /Review move/i }).click();
  const moveResponse = page.waitForResponse(response => response.url().endsWith(movePath) && response.request().method() === "POST");
  await moveReview.getByRole("button", { name: /Confirm move/i }).click();
  const moved = await moveResponse;
  if (moved.status() !== 200 || JSON.stringify(moved.request().postDataJSON()) !== JSON.stringify({ publicationPath: "ssh-readable-site", expectedPublicationPath: intermediatePath, confirm: true })) throw new Error(`dashboard did not confirm the chosen move (${moved.status()})`);
  await commitCard.getByText(`Publication path: ${movedPath}`, { exact: true }).waitFor();
  await waitFor(async () => {
    try { await stat(sshPublishedPath); return false; } catch (error) { if (error.code !== "ENOENT") throw error; }
    try { return (await stat(movedPath)).ino === originalIdentity && (await readFile(join(movedPath, "index.html"), "utf8")) === originalContent && (await readFile(join(movedPath, "asset.dat"), "utf8")) === originalAsset; } catch { return false; }
  }, "moved legacy publication and hydrated content");
  const movedDeployment = await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).find(item => item.id === id), sshDeployment.body.id);
  if (movedDeployment?.publicationPath !== movedPath || movedDeployment.liveCommit !== sshCommit) throw new Error(`moved deployment lost its live release: ${JSON.stringify(movedDeployment)}`);
  await page.reload({ waitUntil: "domcontentloaded" });
  await commitCard.getByText(`Publication path: ${movedPath}`, { exact: true }).waitFor();
  const redeployMoved = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${sshDeployment.body.id}/hard-redeploy`));
  await commitCard.getByRole("button", { name: "Hard re-deploy", exact: true }).click();
  const redeployAccepted = await redeployMoved;
  if (redeployAccepted.status() !== 202) throw new Error(`hard re-deploy after move returned ${redeployAccepted.status()}`);
  const redeployJob = await redeployAccepted.json();
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), sshDeployment.body.id);
    return jobs.some(job => job.id === redeployJob.jobId && job.status === "succeeded") && (await stat(movedPath)).ino !== originalIdentity;
  }, "publication recreated at moved destination");
  if ((await readFile(join(movedPath, "index.html"), "utf8")) !== originalContent || (await readFile(join(movedPath, "asset.dat"), "utf8")) !== originalAsset) throw new Error("deployment after move changed published content");
  const identityBeforeNoOp = (await stat(movedPath)).ino;
  const noOpMove = await page.evaluate(async ([path, currentPath]) => {
    const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ publicationPath: "ssh-readable-site", expectedPublicationPath: currentPath, confirm: true }) });
    return response.status;
  }, [movePath, movedPath]);
  if (![200, 204, 409].includes(noOpMove) || (await stat(movedPath)).ino !== identityBeforeNoOp || (await readFile(join(movedPath, "index.html"), "utf8")) !== originalContent) throw new Error(`same-destination move changed the publication (HTTP ${noOpMove})`);
  let failedDeployAction = false;
  await page.route(`**/api/deployments/${sshDeployment.body.id}/deploy`, async route => {
    failedDeployAction = true;
    await route.fulfill({ status: 500, contentType: "text/plain", body: "SENSITIVE_DEPLOY_FAILURE" });
  });
  await commitCard.getByRole("button", { name: "Deploy", exact: true }).click();
  await commitCard.getByRole("alert").filter({ hasText: "The request could not be completed. Try again shortly." }).waitFor();
  const successfulRefsAfterAction = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/refs`) && response.ok());
  await successfulRefsAfterAction;
  await page.waitForTimeout(150);
  if (!failedDeployAction || !(await commitCard.getByRole("alert").count())) throw new Error("successful refs fetch hid an unrelated deploy error");
  await page.unroute(`**/api/deployments/${sshDeployment.body.id}/deploy`);
  if (await commitCard.getByRole("button", { name: /^(Pause|Resume)$/ }).count()) throw new Error("commit deployment showed branch-only automation controls");
  milestone("deployment and project deletion confirmation");
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByText(/Delete “SSH published site” and remove its published files/).waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  if ((await readFile(join(movedPath, "index.html"), "utf8")) !== "SSH fixture site\n") throw new Error("cancelling deployment deletion changed its publication");
  await page.getByRole("link", { name: "SSH fixture site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${sshProject.id}`);
  await page.getByRole("button", { name: "Delete project" }).click();
  await page.getByText(/Projects with deployments cannot be deleted/).waitFor();
  const blockedProjectDelete = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  if ((await blockedProjectDelete).status() !== 409) throw new Error("project deletion was not blocked while a deployment existed");
  if (!(await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.some(project => project.id === id), sshProject.id))) throw new Error("blocked project deletion removed the project");
  await page.getByRole("link", { name: "SSH published site", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${sshProject.id}/deployments/${sshDeployment.body.id}`);
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const deploymentDelete = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${sshDeployment.body.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  if ((await deploymentDelete).status() !== 204) throw new Error("confirmed deployment deletion did not finish");
  await page.waitForURL(`**/dashboard/projects/${sshProject.id}`);
  await waitFor(async () => { try { await stat(movedPath); return false; } catch (error) { if (error.code === "ENOENT") return true; throw error; } }, "deleted moved publication files");
  if ((await readFile(join(occupiedPath, "keep.txt"), "utf8")) !== "not owned by deployment\n") throw new Error("deleting the moved deployment touched an unrelated occupied folder");
  await page.getByRole("button", { name: "Delete project" }).click();
  await page.getByText(/Projects with deployments cannot be deleted/).waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  if (!(await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.some(project => project.id === id), sshProject.id))) throw new Error("cancelling project deletion changed the project");
  await page.getByRole("button", { name: "Delete project" }).click();
  const projectDelete = page.waitForResponse(response => response.url().endsWith(`/api/projects/${sshProject.id}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  const confirmedProjectDelete = await projectDelete;
  if (confirmedProjectDelete.status() !== 204) throw new Error(`confirmed project deletion returned ${confirmedProjectDelete.status()}: ${await confirmedProjectDelete.text()}`);
  if (await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.some(project => project.id === id), sshProject.id)) throw new Error("deleted project remained visible");
  try { await stat(join(dir, "state.sqlite.projects", sshProject.id)); throw new Error("project cache remains after deletion"); } catch (error) { if (error.message === "project cache remains after deletion") throw error; }
  milestone("deleted and missing project routes");
  await page.goto(`https://${appListen}/dashboard/projects/${sshProject.id}`, { waitUntil: "domcontentloaded" });
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByText(/Project not found|This project no longer exists/i).waitFor();
  await page.getByRole("navigation", { name: "Dashboard" }).getByRole("link", { name: "Projects", exact: true }).click();
  await page.goto(`https://${appListen}/dashboard/projects/missing-project`, { waitUntil: "domcontentloaded" });
  await page.getByText(/Project not found|This project no longer exists/i).waitFor();
  milestone("review and confirm project cache recreation");
  await openProject(page, id, "Fixture site");
  const pinnedDeploymentResponse = await page.evaluate(async ({ id, commit }) => {
    const response = await fetch("/api/deployments", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ projectId: id, name: "Pinned cache check", commit }) });
    return { status: response.status, body: await response.json() };
  }, { id, commit: featureCommit });
  if (pinnedDeploymentResponse.status !== 201) throw new Error(`could not create pinned cache check: ${JSON.stringify(pinnedDeploymentResponse)}`);
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), pinnedDeploymentResponse.body.id);
    return jobs[0]?.status === "succeeded";
  }, "pinned cache check publication");
  const pinnedPublicationPath = join(dir, "state.sqlite.publications", pinnedDeploymentResponse.body.id);
  const pinnedPublicationBeforeCacheRecreation = await readFile(join(pinnedPublicationPath, "index.html"), "utf8");
  const cacheSection = page.getByRole("region", { name: "Repository storage" });
  await cacheSection.getByText(/Git cache: [1-9]/).waitFor();
  await cacheSection.getByRole("button", { name: "Clear cache…" }).click();
  const cacheConfirmation = cacheSection.getByRole("dialog", { name: "Confirm clearing repository storage" });
  if (await page.evaluate(() => document.activeElement?.id) !== "cache-recreate-confirm") throw new Error("cache confirmation did not move focus to its primary action");
  if (await cacheConfirmation.getAttribute("aria-modal") !== "true" || !(await cacheConfirmation.getAttribute("aria-describedby"))) throw new Error("cache confirmation is missing modal or description semantics");
  if (!(await cacheConfirmation.locator('[role="alert"]').count())) throw new Error("cache warning is not announced to assistive technology");
  await cacheConfirmation.getByText(/no longer reachable from a\s+branch or tag/).waitFor();
  await cacheConfirmation.getByText(/server can no longer provide/).waitFor();
  await cacheSection.getByRole("button", { name: "Cancel" }).click();
  await page.waitForFunction(() => document.activeElement?.id === "cache-recreate-trigger");
  if (!(await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id).gitCacheBytes > 0, id))) throw new Error("cancelling cache recreation changed the Git cache");
  const liveBeforeCacheRecreation = await readFile(join(publishedPath, "index.html"), "utf8");
  await cacheSection.getByRole("button", { name: "Clear cache…" }).click();
  const cacheRecreation = page.waitForResponse(response => response.url().endsWith(`/api/projects/${id}/cache/recreate`));
  await cacheSection.getByRole("button", { name: "Clear cache", exact: true }).click();
  const cacheRecreationResponse = await cacheRecreation;
  if (cacheRecreationResponse.status() !== 200 || (await cacheRecreationResponse.json()).code !== "project.cache_cleared") throw new Error(`confirmed cache clearing returned an unexpected outcome (${cacheRecreationResponse.status()})`);
  await page.waitForFunction(() => document.activeElement?.id === "cache-recreate-trigger");
  const recreatedProject = await page.evaluate(async id => (await (await fetch("/api/projects")).json()).data.find(project => project.id === id), id);
  if (recreatedProject.gitCacheBytes !== 0 || recreatedProject.lfsCacheBytes !== 0) throw new Error(`cache sizes after recreation are not zero: ${JSON.stringify(recreatedProject)}`);
  await cacheSection.getByText("Git cache: 0 bytes · LFS cache: 0 bytes").waitFor();
  if ((await readFile(join(publishedPath, "index.html"), "utf8")) !== liveBeforeCacheRecreation) throw new Error("cache recreation changed live published content");
  await page.getByRole("link", { name: "Pinned cache check", exact: true }).click();
  await page.waitForURL(`**/dashboard/projects/${id}/deployments/${pinnedDeploymentResponse.body.id}`);
  let leavingViewHistoryPolls = 0;
  await page.route(`**/api/deployments/${pinnedDeploymentResponse.body.id}/jobs`, async route => {
    if (route.request().method() === "GET") leavingViewHistoryPolls++;
    await route.continue();
  });
  await waitFor(() => leavingViewHistoryPolls > 0, "active deployment history polling before leaving dashboard");
  await page.goto(`https://${appListen}/account/password`, { waitUntil: "domcontentloaded" });
  await page.getByLabel("Current password").waitFor();
  const pollCountAfterLeaving = leavingViewHistoryPolls;
  await new Promise(resolveWait => setTimeout(resolveWait, 2200));
  if (leavingViewHistoryPolls !== pollCountAfterLeaving) throw new Error("deployment history polling continued after leaving the dashboard");
  await page.unroute(`**/api/deployments/${pinnedDeploymentResponse.body.id}/jobs`);
  if (await httpsRequestStatus("127.0.0.1", 18441, "/__fixture/fail", "POST") !== 204) throw new Error("could not make the Git server unavailable for the missing-cache check");
  const deployPinnedAfterCacheClear = await page.evaluate(async id => {
    const response = await fetch(`/api/deployments/${id}/hard-redeploy`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    return { status: response.status, body: await response.json() };
  }, pinnedDeploymentResponse.body.id);
  if (deployPinnedAfterCacheClear.status !== 202) throw new Error(`pinned deployment was not accepted after cache clear: ${JSON.stringify(deployPinnedAfterCacheClear)}`);
  let missingCommitJob;
  try {
    await waitFor(async () => {
      const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), pinnedDeploymentResponse.body.id);
      missingCommitJob = jobs.find(job => job.id === deployPinnedAfterCacheClear.body.jobId);
      return missingCommitJob?.status === "failed";
    }, "pinned commit failure after cache recreation");
  } catch (error) {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), pinnedDeploymentResponse.body.id);
    throw new Error(`${error.message}; job history: ${JSON.stringify(jobs)}`);
  }
  if ((await readFile(join(pinnedPublicationPath, "index.html"), "utf8")) !== pinnedPublicationBeforeCacheRecreation) throw new Error("unavailable pinned commit changed its live publication");
  const deployAfterCacheClear = await page.evaluate(async id => {
    const response = await fetch(`/api/deployments/${id}/deploy`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    return { status: response.status, body: await response.json() };
  }, deploymentID);
  if (deployAfterCacheClear.status !== 202 || !deployAfterCacheClear.body.jobId) throw new Error(`post-clear deployment was not accepted: ${JSON.stringify(deployAfterCacheClear)}`);
  let unavailableJob;
  await waitFor(async () => {
    const jobs = await page.evaluate(async id => await (await fetch(`/api/deployments/${id}/jobs`)).json(), deploymentID);
    unavailableJob = jobs.find(job => job.id === deployAfterCacheClear.body.jobId);
    return unavailableJob?.status === "failed";
  }, "deployment failure after the repository became unavailable");
  if (!unavailableJob.log.includes("current live files were kept")) throw new Error(`unavailable repository failure did not preserve the publication: ${unavailableJob.log}`);
  if ((await readFile(join(publishedPath, "index.html"), "utf8")) !== liveBeforeCacheRecreation) throw new Error("failed recovery after cache recreation changed live published content");
  const unavailableOldCommit = await page.evaluate(async ({ id, commit }) => {
    const response = await fetch("/api/deployments", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ projectId: id, name: "Unavailable cached commit", commit }) });
    return { status: response.status, body: await response.text() };
  }, { id, commit: featureCommit });
  if (unavailableOldCommit.status !== 400 || !unavailableOldCommit.body.includes("commit is not available")) throw new Error(`removed commit remained deployable: ${JSON.stringify(unavailableOldCommit)}`);
  milestone("delete deployment from chosen publication folder");
  await page.goto(`https://${appListen}/dashboard/projects/${id}/deployments/${deploymentID}`, { waitUntil: "domcontentloaded" });
  await page.getByText(`Publication path: ${publishedPath}`, { exact: true }).waitFor();
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByText(/Delete “Production site” and remove its published files/).waitFor();
  const chosenPathDelete = page.waitForResponse(response => response.url().endsWith(`/api/deployments/${deploymentID}/delete`));
  await page.getByRole("button", { name: "Confirm delete" }).click();
  if ((await chosenPathDelete).status() !== 204) throw new Error("chosen publication folder deletion did not finish");
  await page.waitForURL(`**/dashboard/projects/${id}`);
  await waitFor(async () => { try { await stat(publishedPath); return false; } catch (error) { if (error.code === "ENOENT") return true; throw error; } }, "chosen publication folder removal");
  if (await page.evaluate(async id => (await (await fetch("/api/deployments")).json()).some(item => item.id === id), deploymentID)) throw new Error("deleted deployment remained in the list");
  if (pageErrors.length) throw new Error(`browser errors: ${pageErrors.slice(0, 4).join("; ")}`);
  console.log("combined-binary authenticated project workflow passed: readable publication folders, legacy move and deletion, private SSH/LFS publication, cache size/recreation warning and confirmation, restart-safe cache lifecycle, host trust, cross-origin mutation rejection, and cached-ref retention");
  }
} catch (error) {
  if (error === focusedOutcomesComplete) {
    // The focused run completed; the shared finally block still cleans up fixtures.
  } else {
  console.error(`[projects-browser] failed during: ${currentStep}`);
  throw error;
  }
} finally {
  releasePauseFailure?.();
  // A broken browser action or route must not prevent fixture termination.
  try {
    await withDeadline(browser?.close(), "browser cleanup", 5000);
  } finally {
    for (const child of [app, fixture, sshFixture]) {
      if (child && child.exitCode === null && child.signalCode === null) {
        const exited = once(child, "exit");
        child.kill("SIGTERM");
        await Promise.race([exited, new Promise(resolveExit => setTimeout(resolveExit, 1000))]);
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      }
    }
    await rm(dir, { recursive: true, force: true });
  }
}
