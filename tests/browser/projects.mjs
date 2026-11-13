import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { copyFile, mkdtemp, rm, chmod } from "node:fs/promises";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { request } from "node:https";

const root = new URL("../../", import.meta.url).pathname;
const dir = await mkdtemp(join(tmpdir(), "deploy-manager-project-browser-"));
const executable = join(dir, "deploy-manager");
let server;
let output = "";

function status(path, method = "GET", body = "") {
  return new Promise((resolveStatus, reject) => {
    const req = request(`https://127.0.0.1:18439${path}`, { method, rejectUnauthorized: false, headers: { Host: "127.0.0.1:18439", Origin: "https://127.0.0.1:18439", "Content-Type": "application/json" } }, response => {
      response.resume();
      response.on("end", () => resolveStatus(response.statusCode));
    });
    req.on("error", reject);
    req.end(body);
  });
}

try {
  const built = process.env.DEPLOY_MANAGER_BINARY;
  if (!built) throw new Error("DEPLOY_MANAGER_BINARY must point to the combined executable");
  await copyFile(resolve(built), executable);
  await chmod(executable, 0o700);
  const listen = "127.0.0.1:18439";
  const cert = join(dir, "localhost.crt");
  const key = join(dir, "localhost.key");
  const generated = spawnSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert, "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1", "-days", "1"], { encoding: "utf8" });
  if (generated.status !== 0) throw new Error(generated.stderr || "Could not create test certificate");
  server = spawn(executable, ["serve", "--data", dir, "--listen", listen, "--tls-cert", cert, "--tls-key", key, "--origin", `https://${listen}`], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  server.stdout.on("data", chunk => { output += chunk.toString(); });
  server.stderr.on("data", chunk => { output += chunk.toString(); });
  const ready = await new Promise((resolveReady, reject) => {
    const deadline = Date.now() + 10000;
    const poll = async () => {
      try {
        const response = await status("/api/projects");
        if (response === 401) return resolveReady(true);
      } catch {}
      if (Date.now() > deadline) return reject(new Error(`project API did not start: ${output}`));
      setTimeout(poll, 100);
    };
    poll();
  });
  if (!ready) throw new Error("project API readiness check failed");
  const post = await status("/api/projects", "POST", JSON.stringify({ name: "site", url: "https://example.test/site.git" }));
  if (post !== 401) throw new Error(`unauthenticated project creation returned ${post}, expected 401`);
  const plain = await new Promise(resolvePlain => {
    const req = request({ hostname: "127.0.0.1", port: 18439, path: "/api/projects", method: "GET" }, response => {
      response.resume();
      response.on("end", () => resolvePlain(response.statusCode));
    });
    req.on("error", () => resolvePlain(0));
    req.end();
  });
  if (plain === 200) throw new Error("plaintext HTTP bypassed authentication on the HTTPS listener");
  console.log("combined-binary project HTTP boundary passed: unauthenticated access remains protected");
} finally {
  if (server && server.exitCode === null) {
    server.kill("SIGTERM");
    await Promise.race([once(server, "exit"), new Promise(resolveExit => setTimeout(resolveExit, 1000))]);
  }
  await rm(dir, { recursive: true, force: true });
}
