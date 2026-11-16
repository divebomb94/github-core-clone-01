import { createApp } from './app.ts';

const base = (process.env.MAKEFUN_ENV_URL || '').replace(/\/+$/, '');
if (!base) { process.stderr.write('MAKEFUN_ENV_URL is required\n'); process.exit(1); }

const server = createApp();
server.listen(0);
await new Promise<void>((resolve) => server.once('listening', resolve));
const address = server.address();
const port = typeof address === 'object' && address ? address.port : 0;

async function request(path: string, init: RequestInit): Promise<Response> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 30000);
  try { return await fetch(base + path, { ...init, signal: controller.signal }); }
  finally { clearTimeout(timer); }
}
async function pause(ms: number): Promise<void> { await new Promise((r) => setTimeout(r, ms)); }

let delay = 250;
while (true) {
  try {
    const response = await request('/register', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ port }) });
    if (response.status === 200 || response.status === 409) break;
    if (response.status === 400 || response.status === 405) { process.exitCode = 1; server.close(); process.exit(1); }
  } catch {}
  await pause(delay); delay = Math.min(delay * 2, 2000);
}
delay = 250;
while (true) {
  try {
    const response = await request('/done', { method: 'GET' });
    if (response.ok) { const value = await response.json(); if (value && value.done === true) break; }
  } catch {}
  await pause(delay); delay = Math.min(delay * 2, 2000);
}
await new Promise<void>((resolve) => server.close(() => resolve()));
