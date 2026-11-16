import * as http from 'node:http';
import { store } from './store.ts';
import { registerRepoRoutes } from './rest-repos.ts';
import { registerContentRoutes } from './rest-contents.ts';
import { registerIssueRoutes } from './rest-issues.ts';
import { registerPullRoutes } from './rest-pulls.ts';
import { registerGitHttpRoutes } from './rest-git.ts';
import { registerGraphqlRoutes } from './rest-graphql.ts';
import { registerMcpRoutes } from './rest-mcp.ts';

export type Handler = (req: http.IncomingMessage, res: http.ServerResponse, params: Record<string, string>, body: unknown) => void;
export type Router = { register: (method: string, pattern: string, handler: Handler) => void };

export function sendJson(res: http.ServerResponse, status: number, value: unknown, headers: Record<string, string> = {}): void {
  const data = JSON.stringify(value);
  res.writeHead(status, { 'content-type': 'application/json', ...headers });
  res.end(data);
}

function compile(pattern: string): { parts: string[]; names: string[] } {
  const parts = pattern.replace(/^\/|\/$/g, '').split('/').filter(Boolean);
  return { parts, names: parts.filter((p) => p.startsWith(':')).map((p) => p.slice(1)) };
}

export function createApp(): http.Server {
  const routes: Array<{ method: string; pattern: string; compiled: { parts: string[]; names: string[] }; handler: Handler }> = [];
  const router: Router = { register(method, pattern, handler) { routes.push({ method: method.toUpperCase(), pattern, compiled: compile(pattern), handler }); } };
  router.register('GET', '/healthz', (_req, res) => sendJson(res, 200, { ok: true }));
  router.register('GET', '/api/v3', (_req, res) => sendJson(res, 200, {
    current_user_url: '/api/v3/user', repository_url: '/api/v3/repos/{owner}/{repo}', issue_url: '/api/v3/issues',
    pull_request_url: '/api/v3/repos/{owner}/{repo}/pulls', commit_url: '/api/v3/repos/{owner}/{repo}/commits',
    content_url: '/api/v3/repos/{owner}/{repo}/contents', label_url: '/api/v3/labels/{name}', notification_url: '/api/v3/notifications'
  }, { 'X-OAuth-Scopes': 'repo, read:org' }));
  router.register('GET', '/api/v3/', (_req, res) => sendJson(res, 200, {
    current_user_url: '/api/v3/user', repository_url: '/api/v3/repos/{owner}/{repo}', issue_url: '/api/v3/issues',
    pull_request_url: '/api/v3/repos/{owner}/{repo}/pulls', commit_url: '/api/v3/repos/{owner}/{repo}/commits',
    content_url: '/api/v3/repos/{owner}/{repo}/contents', label_url: '/api/v3/labels/{name}', notification_url: '/api/v3/notifications'
  }, { 'X-OAuth-Scopes': 'repo, read:org' }));
  registerRepoRoutes(router, store);
  registerContentRoutes(router, store);
  registerIssueRoutes(router, store);
  registerPullRoutes(router, store);
  registerGitHttpRoutes(router, store);
  registerGraphqlRoutes(router, store);
  registerMcpRoutes(router, store);
  let requestId = 0;
  void store;
  return http.createServer((req, res) => {
    res.setHeader('X-Request-Id', String(++requestId));
    res.setHeader('X-GitHub-Request-Id', String(requestId));
    const chunks: Buffer[] = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('end', () => {
      const path = (req.url || '').split('?')[0];
      let body: unknown = null;
      if (['POST', 'PUT', 'PATCH', 'DELETE'].includes(req.method || '')) {
        try { body = path.endsWith('/git-upload-pack') || path.endsWith('/git-receive-pack') ? Buffer.concat(chunks) : chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : null; }
        catch { sendJson(res, 400, path === '/mcp' ? { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } : { message: 'Problems parsing JSON' }); return; }
      }
      if (path !== '/healthz') {
        const expected = `Bearer ${process.env.MAKEFUN_SHOP_TOKEN ?? ''}`;
        if (req.headers.authorization !== expected) { sendJson(res, 401, { message: 'Bad credentials' }); return; }
      }
      const pathParts = path.replace(/^\/|\/$/g, '').split('/').filter(Boolean);
      const route = routes.find((r) => {
        if (r.method !== (req.method || '').toUpperCase()) return false;
        const parts = r.compiled.parts; const multi = parts[parts.length - 1]?.endsWith('+');
        if (multi) return pathParts.length >= parts.length - 1 && parts.slice(0, -1).every((p, i) => p.startsWith(':') || p === pathParts[i]);
        return parts.length === pathParts.length && parts.every((p, i) => p.startsWith(':') || p === pathParts[i]);
      });
      if (!route) { sendJson(res, 404, { message: 'Not Found' }); return; }
      const params: Record<string, string> = {}; route.compiled.parts.forEach((p, i) => { if (p.startsWith(':')) { const multi = p.endsWith('+'); const key = p.slice(1, multi ? -1 : undefined); params[key] = decodeURIComponent((multi ? pathParts.slice(i) : [pathParts[i]]).join('/')); } });
      route.handler(req, res, params, body);
    });
  });
}
