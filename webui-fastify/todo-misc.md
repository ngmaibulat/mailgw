Todo: convert bcrypt compare to async

Why
- `bcrypt.compareSync` blocks the event loop during password verification. For safety and scalability, use an async compare that returns a Promise.

Suggested minimal implementation
- Add a small promise-wrapper in `src/adapter.ts` that exports `bcryptCompare(pass, hash): Promise<boolean>`.
- Replace `bcrypt.compareSync(pass, hash)` in `src/auth/util.ts` with `await bcryptCompare(pass, hash)`.

Tests to add
- webui-fastify/tests/auth.util.test.ts
  - stub DB select to return a fake user record with a hash
  - stub adapter.bcryptCompare to resolve true/false and assert checkAuth result

Notes
- Keep this change isolated to the adapter so callers don't need to be refactored.
- No behavioral change for callers, only non-blocking I/O.
