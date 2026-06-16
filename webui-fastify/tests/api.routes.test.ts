import { test } from 'node:test';
import assert from 'node:assert/strict';

// Instead of building the full app (which registers the logger hook that tries
// to write to the DB), call the route factory with a fake fastify object to
// capture the handler and invoke it directly.
import apiRoutes from '../src/routes/api.ts';

test('api routes proxy to logservice and return JSON', async () => {
  // stub fetch so logservice.search will return expected JSON
  (globalThis as any).fetch = async (url: URL | string, opts?: any) => ({
    ok: true,
    status: 200,
    json: async () => ({ status: 'success', total: 1, records: [{ id: 1 }] }),
  } as any);

  const handlers: Record<string, Function> = {};
  const fakeFastify: any = {
    get: (path: string, handler: Function) => { handlers[path] = handler; },
  };

  await apiRoutes(fakeFastify as any);

  // ensure the handler exists
  const connHandler = handlers['/connection'];
  assert.ok(connHandler, 'connection handler registered');

  const fakeReq: any = { query: { request: '{}' } };
  const fakeReply: any = { send: (v: any) => v };

  const result = await connHandler(fakeReq, fakeReply);
  assert.strictEqual(result.status, 'success');
  assert.strictEqual(result.total, 1);
});
