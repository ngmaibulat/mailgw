import { test } from 'node:test';
import assert from 'node:assert/strict';

test('logservice.search sets q param and X-API-Key', async () => {
  const calls: Array<{ url: string; headers: Record<string,string> }> = [];

  // stub fetch
  (globalThis as any).fetch = async (url: URL | string, opts?: any) => {
    calls.push({ url: String(url), headers: opts?.headers ?? {} });
    return {
      ok: true,
      status: 200,
      json: async () => ({ status: 'success', total: 0, records: [] }),
    } as any;
  };

  // without API key, header should be absent
  delete process.env.LOGSERVICE_API_KEY;
  // import after ensuring env state so the module's cached API_KEY reflects it
  const logservice = await import('../src/logservice.ts?case=without');
  await logservice.search('/api/connection', '{"foo":1}');
  assert.strictEqual(calls.length, 1);
  assert.ok(calls[0].url.includes('q=%7B%22foo%22%3A1%7D'), 'q param encoded');

  // with API key, header included
  calls.length = 0;
  process.env.LOGSERVICE_API_KEY = 'sekret';
  // re-import with a distinct specifier so Node will reload the module and pick up the new env
  const logservice2 = await import('../src/logservice.ts?case=with');
  await logservice2.search('/api/connection', '{"bar":2}');
  assert.strictEqual(calls.length, 1);
  const headersObj = calls[0].headers;
  // headers may be a plain object or a Headers-like instance
  const gotApiKey =
    typeof headersObj.get === 'function'
      ? headersObj.get('X-API-Key') || headersObj.get('x-api-key')
      : headersObj['X-API-Key'] || headersObj['x-api-key'];
  assert.strictEqual(gotApiKey, 'sekret');
});
