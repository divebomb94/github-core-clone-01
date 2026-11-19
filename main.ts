// Resolve forge beside main.ts first, independently of the launcher's cwd.
// Try each candidate fully, using a temp copy if its mode cannot be fixed;
// remove any temp copy when the child exits.
import { spawn } from 'node:child_process';
import {
  accessSync,
  chmodSync,
  constants,
  copyFileSync,
  mkdtempSync,
  rmSync,
  statSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const candidates = [...new Set([join(scriptDir, 'forge'), resolve(process.cwd(), 'forge')])];
let tempDirectory;

function cleanupTemp() {
  if (tempDirectory) {
    try {
      rmSync(tempDirectory, { recursive: true, force: true });
    } catch {}
    tempDirectory = undefined;
  }
}

function findForge() {
  const failures = [];
  for (const path of candidates) {
    try {
      if (!statSync(path, { throwIfNoEntry: false })?.isFile()) {
        throw new Error('not a file or does not exist');
      }
      return executablePath(path);
    } catch (err) {
      cleanupTemp();
      failures.push(`${path}: ${err.message}`);
    }
  }
  throw new Error(`all candidates failed; tried: ${failures.join('; ')}`);
}

function executablePath(path) {
  try {
    accessSync(path, constants.X_OK);
    return path;
  } catch {
    try {
      chmodSync(path, 0o755);
      accessSync(path, constants.X_OK);
      return path;
    } catch {
      tempDirectory = mkdtempSync(join(tmpdir(), 'forge-'));
      const copy = join(tempDirectory, 'forge');
      copyFileSync(path, copy);
      chmodSync(copy, 0o755);
      return copy;
    }
  }
}

let binary;
try {
  binary = findForge();
} catch (err) {
  console.error(`forge: cannot prepare binary${binary ? ` ${binary}` : ''}: ${err.message}`);
  process.exit(1);
}

function reportSpawnError(err) {
  console.error(`forge: failed to start ${binary}: ${err.message}`);
  process.exit(1);
}

let child;
try {
  child = spawn(binary, [], {
    env: process.env,
    stdio: 'inherit',
  });
} catch (err) {
  reportSpawnError(err);
}

child.on('error', reportSpawnError);

child.on('exit', (code, signal) => {
  cleanupTemp();
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});
