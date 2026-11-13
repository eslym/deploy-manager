import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { delimiter, join } from 'node:path';
import { spawnSync } from 'node:child_process';

const temp = mkdtempSync(join(tmpdir(), 'opaque-browser-gate-'));
try {
  const calls = join(temp, 'calls');
  const fakeBin = join(temp, 'bin');
  const fakeBun = join(fakeBin, 'bun');
  await Bun.write(fakeBun, `#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> "$OPAQUE_GATE_CALLS"\nif [[ "$*" == "run audit-artifact" ]]; then exit 41; fi\nexit 0\n`);
  chmodSync(fakeBun, 0o755);

  const result = spawnSync('bash', ['run-browser.sh'], {
    cwd: import.meta.dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      PATH: `${fakeBin}${delimiter}${process.env.PATH}`,
      OPAQUE_GATE_CALLS: calls,
    },
  });

  assert.equal(result.status, 41, `expected audit failure to stop browser startup; stderr: ${result.stderr}`);
  assert.deepEqual(readFileSync(calls, 'utf8').trim().split('\n'), ['run audit-artifact']);
} finally {
  rmSync(temp, { recursive: true, force: true });
}

console.log('browser entrypoint stops after a failed artifact audit');
