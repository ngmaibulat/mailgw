# Code Review — 2026-06-08

## 1. Riskiest Areas to Change

**`plugins/Route.js` — `getCheckerFunction` (line 33)**
The wildcard logic relies on `param.toString()` being falsy for empty-string wildcards. If a config value is `null` instead of `""`, `null.toString()` throws immediately. If it's the number `0`, `"0"` is truthy and will match only the literal string `"0"`. Any edit to the wildcard/matching logic risks breaking all routing silently with no test coverage.

**`plugins/npRoute.js` — `hook_get_mx` (line 48)**
When `rtable.findRoute()` returns `false` (no route matched, or misconfigured relay), the code still calls `return next(OK, false)`. Passing `false` as the relay object to Haraka's MX handler is undocumented behavior — it will likely crash or misdeliver mail. Changing the routing or error path here requires care.

**`plugins/npFilter.js` — `hook_connect` (line 49)**
IP filtering is the only security gate for inbound connections. If `cfg.allowed` is missing or not an array (malformed config), `cfg.allowed.includes(...)` throws a TypeError that crashes the hook — Haraka's behavior on a crashed hook is to fall through to the next plugin, which could allow the connection. Any change to config loading or the allowlist logic here is high-risk.

**`logservice/src/functions.js` — `createQuery` / `getData` (lines 30–143)**
The `item.field` value from the request is used directly as a Sequelize column name with no allowlist validation (`res[item.field] = {}`). This is the query-builder path. Changing the search/filter logic risks introducing column-injection or breaking all search queries.

---

## 2. Bugs and Security Issues

**Bug — `hook_get_mx` passes `false` to `next(OK, ...)`** (`plugins/npRoute.js:66`)
When no route matches, `relay` is `false`. `return next(OK, false)` tells Haraka the lookup succeeded with `false` as the MX record, which is undefined behavior. It should call `return next(DENYSOFT)` or similar to signal a routing failure.

**Security — Zod-validated body not used** (`logservice/src/routes/api.js:13`)
```js
const parsed = schemaDelivery.safeParse(req.body);
if (parsed.success) {
    models.Delivery.create(req.body);  // should be parsed.data
```
Zod validates `req.body` but `parsed.data` (the sanitized, coerced output) is discarded. The raw `req.body` — which may contain extra fields — is passed to `Sequelize.create()`. This allows callers to inject arbitrary fields into the INSERT.

**Security — No authentication on logservice** (`logservice/src/routes/api.js`)
All three POST endpoints (`/connection`, `/queue`, `/delivery`) accept data from anyone who can reach port 3000. There's no API key, token, or network-level restriction enforced in code. If the logservice is ever accidentally exposed, anyone can write delivery records.

**Security — Unvalidated dynamic field names in query builder** (`logservice/src/functions.js:67`)
`item.field` from the request body is used as a raw key: `res[item.field][Op.startsWith] = ...`. There's no allowlist of valid column names. A crafted request could target internal Sequelize properties or unexpected columns.

**Bug — `npFilter.js` crashes on missing/malformed config** (`plugins/npFilter.js:55`)
`cfg.allowed.includes(...)` has no null check. If `ngmfilter.json` is absent, malformed, or missing the `allowed` key, this throws on every inbound connection with no recovery.

**Bug — Silent log errors everywhere** (`plugins/functions.js:40`, `plugins/npFilter.js:110`)
Every `fs.appendFile` call has an empty error handler. Log write failures are completely invisible — you'd have no idea logs stopped working.

**Reliability — `hook_get_mx` only checks `rcpt_to[0]`** (`plugins/npRoute.js:50`)
`hmail.todo.rcpt_to[0]` — if `rcpt_to` is empty (edge case in a bounce or malformed message), this will throw a TypeError and crash the outbound queue worker for that message.
