import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkSession } from '../src/middleware/checkSession.ts';
import { sessions } from '../src/globals.ts';

test('checkSession redirects when unauthenticated', async () => {
  // clean sessions
  for (const k of Object.keys(sessions)) delete sessions[k];

  let redirected = false;
  const req: any = { cookies: {}, unsignCookie: (_: any) => ({ valid: false, value: null }), headers: {} };
  const reply: any = { redirect: (u: string) => { redirected = true; assert.strictEqual(u, '/login'); } };

  await checkSession(req, reply);
  assert.ok(redirected, 'should redirect to /login when no session');
});

test('checkSession allows valid session', async () => {
  const id = 'sess-1';
  sessions[id] = { email: 'a@b' };

  const req: any = {
    cookies: { session: id },
    unsignCookie: (raw: any) => ({ valid: true, value: raw }),
    headers: {},
  };
  const reply: any = { redirect: (_: string) => { throw new Error('unexpected redirect'); } };

  // should not throw or redirect
  await checkSession(req, reply);
  // cleanup
  delete sessions[id];
});
