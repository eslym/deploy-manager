const status = document.querySelector("#status");
const form = document.querySelector("#registration");
const passwordInput = document.querySelector("#password");
const token = location.hash.slice(1);
if (token) history.replaceState(null, "", `${location.pathname}${location.search}`);
const profile = { "argon2id-custom": { iterations: 1, memory: 65536, parallelism: 4 } };
async function post(path, value) {
  const response = await fetch(path, {
    method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ grant: token, value })
  });
  if (!response.ok) throw new Error(response.status === 410 ? "This setup link has expired or was already used. Issue a new one with the local CLI." : "Secure registration could not be completed.");
  return response.status === 204 ? null : response.json();
}
try {
  if (!token) throw new Error("This setup link is incomplete. Issue a new one with the local CLI.");
  const opaque = await import("/assets/opaque.js");
  await opaque.ready;
  if (typeof opaque.client?.startRegistration !== "function") throw new Error("Secure registration could not initialize. Check that JavaScript and WebAssembly are enabled, then reload this page.");
  form.hidden = false;
  status.textContent = "Your password stays in this browser.";
  form.addEventListener("submit", async event => {
    event.preventDefault();
    const password = passwordInput.value;
    passwordInput.value = "";
    form.querySelector("button").disabled = true;
    try {
      const start = opaque.client.startRegistration({ password });
      const { value: response } = await post("/opaque/register/start", start.registrationRequest);
      const finish = opaque.client.finishRegistration({ password, registrationResponse: response, clientRegistrationState: start.clientRegistrationState, keyStretching: profile });
      passwordInput.value = "";
      await post("/opaque/register/finish", finish.registrationRecord);
      status.textContent = "Administrator account created. Registration does not sign you in.";
      form.remove();
    } catch (error) {
      status.textContent = error instanceof Error ? error.message : "Secure registration failed. Reload and try again.";
      form.querySelector("button").disabled = false;
    }
  });
} catch (error) {
  status.textContent = error instanceof Error && error.message.includes("setup link is incomplete")
    ? error.message
    : "Secure registration could not initialize. Check that JavaScript and WebAssembly are enabled, then reload this page or issue a new setup link with the local CLI.";
}
