import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { chromium } from "playwright";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const root = new URL("../../", import.meta.url).pathname;
const dir = await mkdtemp(join(tmpdir(), "deploy-manager-browser-"));
const executable = join(dir, "deploy-manager");
let server;
let browser;
let serverOutput = "";
try {
  const build = spawnSync("go", ["build", "-o", executable, "."], { cwd: root, env: { ...process.env, GOTOOLCHAIN: "go1.26.11" }, encoding: "utf8" });
  if (build.status !== 0) throw new Error(build.stderr || "Go build failed");
  const listen = "127.0.0.1:18437";
  const setup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { encoding: "utf8" });
  if (setup.status !== 0) throw new Error(setup.stderr || "CLI setup failed");
  const link = setup.stdout.trim();
  const issuedURL = new URL(link);
  const grant = issuedURL.hash.slice(1);
  if (issuedURL.origin !== "http://127.0.0.1:18437" || issuedURL.pathname !== "/register" || issuedURL.search || !grant) throw new Error("setup CLI did not issue a fragment-only loopback setup URL");
  const failedInitSetup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { encoding: "utf8" });
  if (failedInitSetup.status !== 0) throw new Error(failedInitSetup.stderr || "second setup grant failed");
  const failedInitLink = failedInitSetup.stdout.trim();
  const startServer = async () => {
    server = spawn(executable, ["serve", "--data", dir, "--listen", listen], { stdio: ["ignore", "pipe", "pipe"] });
    server.stdout.on("data", chunk => { serverOutput += chunk.toString(); });
    server.stderr.on("data", chunk => { serverOutput += chunk.toString(); });
    await new Promise((resolve, reject) => {
    const started = Date.now();
    const poll = async () => {
      try { const r = await fetch(`http://${listen}/`); if (r.status === 404) return resolve(); }
      catch {}
      if (Date.now() - started > 10000) return reject(new Error("application server did not start"));
      setTimeout(poll, 100);
    };
    poll();
    });
  };
  await startServer();
  const browserPath = process.env.CHROMIUM_PATH || chromium.executablePath();
  browser = await chromium.launch({ headless: true, executablePath: browserPath, args: ["--no-sandbox"] });
  const context = await browser.newContext();
  const observedURLs = [];
  context.on("request", request => observedURLs.push(request.url()));
  const failedInitPage = await context.newPage();
  let initRequests = 0;
  failedInitPage.on("request", request => { if (request.method() === "POST") initRequests++; });
  await failedInitPage.route("**/assets/opaque.js", route => route.abort());
  await failedInitPage.goto(failedInitLink);
  await failedInitPage.getByText(/Secure registration could not initialize/).waitFor({ timeout: 30000 });
  if (initRequests !== 0) throw new Error("initialization failure attempted to send a password or registration request");
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
  page.on("request", request => { if (request.method() === "POST") postedBodies.push(request.postData() || ""); });
  let finishRaceStatuses;
  await page.route("**/opaque/register/finish", async route => {
    const results = await Promise.all([route.fetch(), route.fetch()]);
    finishRaceStatuses = results.map(result => result.status()).sort((a, b) => a - b);
    const accepted = results.find(result => result.status() === 204);
    if (!accepted) throw new Error(`registration race had no accepted request: ${finishRaceStatuses}`);
    await route.fulfill({ response: accepted });
  });
  await page.goto(link);
  if (new URL(page.url()).hash || !page.url().endsWith("/register")) throw new Error("browser did not remove the grant fragment from the visible URL");
  await page.getByLabel("Password").waitFor({ state: "visible", timeout: 30000 });
  await page.getByLabel("Password").fill("Correct horse battery staple 2026!");
  await page.getByRole("button", { name: "Create administrator" }).click();
  await page.getByText("Administrator account created. Registration does not sign you in.").waitFor({ timeout: 60000 });
  if (JSON.stringify(finishRaceStatuses) !== JSON.stringify([204, 410])) throw new Error(`grant consumption race was not single-use: ${finishRaceStatuses}`);
  if (observedURLs.some(url => url.includes(grant))) throw new Error("registration grant leaked into a browser request URL");
  if (!postedBodies.length || postedBodies.some(body => !body.includes(grant))) throw new Error("grant was not sent with protected registration POST bodies");
  if (await context.cookies().then(c => c.length)) throw new Error("registration unexpectedly issued a session cookie");
  for (const body of postedBodies) {
    if (body.includes("Correct horse battery staple") || body.includes("clientRegistrationState") || body.includes("exportKey") || body.includes("sessionKey")) {
      throw new Error("browser transmitted a password or local OPAQUE secret state");
    }
  }
  const setupReuse = await fetch(link);
  if (setupReuse.status !== 404) throw new Error(`registration should be disabled after bootstrap (got ${setupReuse.status})`);
  const issuedTokens = [grant, new URL(failedInitLink).hash.slice(1)];
  if (issuedTokens.some(token => serverOutput.includes(token))) throw new Error("setup grants appeared in routine application logs");
  server.kill("SIGTERM");
  await once(server, "exit");
  await startServer();
  const afterRestart = await fetch(link);
  if (afterRestart.status !== 404) throw new Error(`registration state did not survive restart (got ${afterRestart.status})`);
  const secondSetup = spawnSync(executable, ["setup", "--data", dir, "--listen", listen], { encoding: "utf8" });
  if (secondSetup.status === 0) throw new Error("CLI issued a new grant after administrator registration");
  await context.close();
  console.log("browser registration passed: failure is fail-closed, OPAQUE completed, no password/session issued, state survives restart");
} finally {
  await browser?.close();
  if (server && server.exitCode === null) { server.kill("SIGTERM"); await Promise.race([once(server, "exit"), new Promise(resolve => setTimeout(resolve, 1000))]); }
  await rm(dir, { recursive: true, force: true });
}
