import type { IncomingMessage, ServerResponse } from 'node:http';
import { sendJson, type Handler, type Router } from './app.ts';
import type { Store } from './store.ts';
import { registerRepoRoutes } from './rest-repos.ts';
import { registerIssueRoutes } from './rest-issues.ts';
import { registerPullRoutes } from './rest-pulls.ts';

const definitions = [
  { name: 'create_repo', aliases: ['create_repository', 'repo_create', 'repos_create', 'repository_create'], description: 'Create a repository under the authenticated viewer.', method: 'POST', path: '/api/v3/user/repos', properties: { name: 'string', description: 'string', private: 'boolean' }, required: ['name'] },
  { name: 'get_repo', aliases: ['get_repository', 'repo_get', 'repos_get', 'repository_get'], description: 'Get a repository.', method: 'GET', path: '/api/v3/repos/:owner/:repo', properties: { owner: 'string', repo: 'string' }, required: ['owner', 'repo'] },
  { name: 'create_issue', aliases: ['issue_create', 'issues_create'], description: 'Create an issue in a repository.', method: 'POST', path: '/api/v3/repos/:owner/:repo/issues', properties: { owner: 'string', repo: 'string', title: 'string', body: 'string' }, required: ['owner', 'repo', 'title'] },
  { name: 'list_issues', aliases: ['issues_list', 'issue_list'], description: 'List repository issues, optionally filtered by state.', method: 'GET', path: '/api/v3/repos/:owner/:repo/issues', properties: { owner: 'string', repo: 'string', state: 'string' }, required: ['owner', 'repo'] },
  { name: 'create_pull_request', aliases: ['pull_request_create', 'pulls_create', 'create_pr', 'pr_create'], description: 'Open a pull request from head into base.', method: 'POST', path: '/api/v3/repos/:owner/:repo/pulls', properties: { owner: 'string', repo: 'string', title: 'string', head: 'string', base: 'string', body: 'string' }, required: ['owner', 'repo', 'title', 'head', 'base'] },
  { name: 'merge_pull_request', aliases: ['pull_request_merge', 'pulls_merge', 'merge_pr', 'pr_merge'], description: 'Merge an open pull request.', method: 'PUT', path: '/api/v3/repos/:owner/:repo/pulls/:number/merge', properties: { owner: 'string', repo: 'string', pull_number: 'integer' }, required: ['owner', 'repo', 'pull_number'] },
] as const;

// Construct once: list responses never depend on request or repository state.
const tools = definitions.flatMap(definition => [definition.name, ...definition.aliases].map(name => ({
  name, description: definition.description,
  inputSchema: { type: 'object', properties: Object.fromEntries(Object.entries(definition.properties).map(([key, type]) => [key, { type }])), required: definition.required },
})));

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export function registerMcpRoutes(router: Router, store: Store): void {
  // Invoke the same handlers as REST, including pullJson and mergePull. Each call
  // gets its own response capture; no HTTP loopback or shared request state.
  const handlers = new Map<string, Handler>();
  const restRouter: Router = { register(method, path, handler) { handlers.set(`${method} ${path}`, handler); } };
  registerRepoRoutes(restRouter, store);
  registerIssueRoutes(restRouter, store);
  registerPullRoutes(restRouter, store);

  router.register('POST', '/mcp', (req, res, _params, body) => {
    const id = isRecord(body) && (typeof body.id === 'string' || typeof body.id === 'number') ? body.id : null;
    const error = (code: number, message: string) => sendJson(res, 200, { jsonrpc: '2.0', id, error: { code, message } });
    const result = (value: unknown) => sendJson(res, 200, { jsonrpc: '2.0', id, result: value });
    if (!isRecord(body) || body.jsonrpc !== '2.0' || typeof body.method !== 'string') return error(-32600, 'Invalid Request');
    const params = isRecord(body.params) ? body.params : {};
    if (body.method === 'initialize') return result({ protocolVersion: params.protocolVersion ?? '2025-11-25', capabilities: { tools: {} }, serverInfo: { name: 'github-forge', version: '1.0.0' } });
    if (body.method === 'notifications/initialized' && !('id' in body)) { res.writeHead(202); res.end(); return; }
    if (body.method === 'tools/list') return result({ tools });
    if (body.method !== 'tools/call') return error(-32601, 'Method not found');
    const definition = definitions.find(tool => [tool.name, ...tool.aliases].some(name => name === params.name));
    if (!definition) return error(-32601, 'Unknown tool');
    const args = params.arguments;
    if (!isRecord(args)) return error(-32602, 'Invalid arguments');
    for (const [key, type] of Object.entries(definition.properties)) {
      const value = args[key];
      if (value === undefined && !(definition.required as readonly string[]).includes(key)) continue;
      if (type === 'integer' ? !Number.isSafeInteger(value) || Number(value) < 1 : typeof value !== type || (type === 'string' && (definition.required as readonly string[]).includes(key) && !String(value).trim())) return error(-32602, `Invalid argument: ${key}`);
    }
    const handler = handlers.get(`${definition.method} ${definition.path}`)!;
    const routeParams = { owner: String(args.owner ?? ''), repo: String(args.repo ?? ''), number: String(args.pull_number ?? '') };
    let status = 200;
    const capture = {
      setHeader() {},
      writeHead(code: number) { status = code; },
      end(data: string) { result({ content: [{ type: 'text', text: data }], isError: status >= 400 }); },
    } as unknown as ServerResponse;
    const url = `/api/v3/repos/${encodeURIComponent(routeParams.owner)}/${encodeURIComponent(routeParams.repo)}/issues?state=${encodeURIComponent(String(args.state ?? 'open'))}&per_page=100`;
    const request = { headers: req.headers, url, method: definition.method } as IncomingMessage;
    try { handler(request, capture, routeParams, args); }
    catch (cause) { result({ content: [{ type: 'text', text: JSON.stringify({ message: cause instanceof Error ? cause.message : 'Tool failed' }) }], isError: true }); }
  });
}
