import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';

const name = '@serenity-kit/opaque';
const version = '1.1.0';
const meta = await (await fetch(`https://registry.npmjs.org/${encodeURIComponent(name)}/${version}`)).json();
assert.equal(meta.version, version);
const tarball = new Uint8Array(await (await fetch(meta.dist.tarball)).arrayBuffer());
const sha512 = new Bun.CryptoHasher('sha512').update(tarball).digest('base64');
const sha512Hex = new Bun.CryptoHasher('sha512').update(tarball).digest('hex');
assert.equal(`sha512-${sha512}`, meta.dist.integrity);
const tmp = mkdtempSync(join(tmpdir(), 'opaque-registry-audit-'));
try {
  const archive = join(tmp, 'artifact.tgz');
  await Bun.write(archive, tarball);
  const files = {};
  for (const file of ['esm/index.js', 'cjs/index.js']) {
    const extracted = spawnSync('tar', ['-xzOf', archive, `package/${file}`]);
    assert.equal(extracted.status, 0, extracted.stderr.toString());
    files[file] = {
      bytes: extracted.stdout.length,
      sha256: new Bun.CryptoHasher('sha256').update(extracted.stdout).digest('hex'),
    };
  }
  assert.equal(files['esm/index.js'].sha256, '1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52');
  assert.equal(files['cjs/index.js'].sha256, '903c6cfa9ee052a04102e21ea4f2f36da753572618765bb21a1a5cccd1b00442');
  const attest = await (await fetch(`https://registry.npmjs.org/-/npm/v1/attestations/${encodeURIComponent(name)}@${version}`)).json();
  const provenance = attest.attestations.find(x => x.predicateType === 'https://slsa.dev/provenance/v1');
  assert.ok(provenance, 'registry SLSA provenance attestation missing');
  const statement = JSON.parse(Buffer.from(provenance.bundle.dsseEnvelope.payload, 'base64').toString());
  const subject = statement.subject.find(x => x.name === `pkg:npm/%40serenity-kit/opaque@${version}`);
  assert.equal(subject?.digest?.sha512, sha512Hex);
  const build = statement.predicate.buildDefinition;
  console.log(JSON.stringify({
    name, version, license: meta.license, tarballBytes: tarball.length,
    distIntegrity: meta.dist.integrity, tarballMatchesRegistryIntegrity: true,
    files, declaredRuntimeDependencies: meta.dependencies ?? {},
    provenance: {
      subjectMatchesTarball: true,
      repository: build.externalParameters.workflow.repository,
      workflow: build.externalParameters.workflow.path,
      workflowRef: build.externalParameters.workflow.ref,
      sourceCommit: build.resolvedDependencies.find(x => x.uri.startsWith('git+'))?.digest?.gitCommit,
      invocation: statement.predicate.runDetails.metadata.invocationId,
      registrySignaturePresent: true,
      localCryptographicSignatureVerification: 'not performed',
    },
  }, null, 2));
} finally {
  rmSync(tmp, { recursive: true, force: true });
}
