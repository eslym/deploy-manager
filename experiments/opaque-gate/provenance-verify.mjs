import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createVerify } from 'node:crypto';

// Cryptographic verification for the exact npm release under review. The
// official cosign binary uses its embedded Sigstore trusted roots and verifies
// the Fulcio chain, DSSE signature, Rekor inclusion proof and checkpoint.
const name = '@serenity-kit/opaque';
const version = '1.1.0';
const pkg = await (await fetch(`https://registry.npmjs.org/${encodeURIComponent(name)}/${version}`)).json();
assert.equal(pkg.name, name);
assert.equal(pkg.version, version);
const tarball = new Uint8Array(await (await fetch(pkg.dist.tarball)).arrayBuffer());
const integrity = `sha512-${new Bun.CryptoHasher('sha512').update(tarball).digest('base64')}`;
assert.equal(integrity, pkg.dist.integrity, 'tarball must first match exact npm registry integrity');

// Verify npm's registry signature over its documented package@version:integrity
// message. Resolve the key by keyid from the registry's HTTPS key endpoint.
const signature = pkg.dist.signatures?.find((s) => s.keyid);
assert.ok(signature, 'npm package signature missing');
const keys = await (await fetch('https://registry.npmjs.org/-/npm/v1/keys')).json();
const key = keys.keys.find((k) => k.keyid === signature.keyid);
assert.ok(key, `npm signing key ${signature.keyid} not published by registry`);
assert.equal(key.keytype, 'ecdsa-sha2-nistp256');
assert.equal(key.scheme, 'ecdsa-sha2-nistp256');
const publicKey = `-----BEGIN PUBLIC KEY-----\n${key.key.match(/.{1,64}/g).join('\n')}\n-----END PUBLIC KEY-----\n`;
const signedMessage = `${name}@${version}:${integrity}`;
const npmVerifier = createVerify('SHA256');
npmVerifier.update(signedMessage);
npmVerifier.end();
const npmSignatureVerified = npmVerifier.verify(publicKey, Buffer.from(signature.sig, 'base64'));
assert.ok(npmSignatureVerified, 'npm package signature did not verify');

const response = await fetch(`https://registry.npmjs.org/-/npm/v1/attestations/${encodeURIComponent(name)}@${version}`);
assert.ok(response.ok, `npm attestations request failed: ${response.status}`);
const responseJSON = await response.json();
const attestation = responseJSON.attestations.find((a) => a.predicateType === 'https://slsa.dev/provenance/v1');
assert.ok(attestation, 'SLSA provenance missing');
const statement = JSON.parse(Buffer.from(attestation.bundle.dsseEnvelope.payload, 'base64').toString());
const digestHex = new Bun.CryptoHasher('sha512').update(tarball).digest('hex');
assert.ok(statement.subject.some((s) => s.name === `pkg:npm/%40serenity-kit/opaque@${version}` && s.digest?.sha512 === digestHex), 'SLSA subject does not identify downloaded tarball');
const workflow = statement.predicate.buildDefinition.externalParameters.workflow;
assert.equal(workflow.repository, 'https://github.com/serenity-kit/opaque');
assert.equal(workflow.path, '.github/workflows/release.yml');
assert.equal(workflow.ref, 'refs/heads/main');
const sourceCommit = statement.predicate.buildDefinition.resolvedDependencies.find((d) => d.uri.startsWith('git+'))?.digest?.gitCommit;
assert.equal(sourceCommit, 'ca1cb22f03b9e456159ce367e2975124f87f5ad8');

const dir = mkdtempSync(join(tmpdir(), 'opaque-provenance-'));
try {
  const artifact = join(dir, 'opaque-1.1.0.tgz');
  const bundlePath = join(dir, 'slsa.bundle.json');
  await Bun.write(artifact, tarball);
  await Bun.write(bundlePath, JSON.stringify(attestation.bundle));
  const cosign = process.env.COSIGN ?? 'cosign';
  const identity = 'https://github.com/serenity-kit/opaque/.github/workflows/release.yml@refs/heads/main';
  const result = spawnSync(cosign, [
    'verify-blob-attestation', '--bundle', bundlePath,
    '--type', 'slsaprovenance1',
    '--certificate-identity', identity,
    '--certificate-oidc-issuer', 'https://token.actions.githubusercontent.com',
    '--certificate-github-workflow-repository', 'serenity-kit/opaque',
    '--certificate-github-workflow-ref', 'refs/heads/main',
    '--certificate-github-workflow-sha', sourceCommit,
    artifact,
  ], { encoding: 'utf8' });
  if (result.error) throw new Error(`Could not execute ${cosign}: ${result.error.message}`);
  if (result.status !== 0) throw new Error(`cosign verification failed (exit ${result.status})\n${result.stdout}\n${result.stderr}`);
  console.log(JSON.stringify({
    package: `${name}@${version}`,
    tarballBytes: tarball.length,
    integrity,
    npmSignature: { verified: true, keyid: signature.keyid, source: 'registry.npmjs.org HTTPS key endpoint' },
    slsa: {
      verified: true,
      verifier: cosign,
      trustedRoots: 'cosign embedded Sigstore trusted root (Fulcio/Rekor)',
      certificateIdentity: identity,
      issuer: 'https://token.actions.githubusercontent.com',
      repository: workflow.repository,
      workflow: workflow.path,
      workflowRef: workflow.ref,
      sourceCommit,
      invocation: statement.predicate.runDetails.metadata.invocationId,
      subjectMatchesDownloadedTarball: true,
    },
  }, null, 2));
} finally {
  rmSync(dir, { recursive: true, force: true });
}
