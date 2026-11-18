import { spawn } from 'node:child_process';

const child = spawn('./forge', [], {
  env: process.env,
  stdio: 'inherit',
});

child.on('error', (err) => {
  console.error('forge: failed to start: ' + err.message);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});
