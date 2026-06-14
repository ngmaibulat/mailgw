# Haraka plugin review (2026-06-14)

Findings from reviewing `plugins/`, prioritized. Nothing here is fixed yet.

## Security / correctness — highest priority

1. **`npRoute.hook_connect` unconditionally enables relaying — defeats the allowlist** (`npRoute.js:71`)

   ```js
   exports.hook_connect = function (next, connection) {
     connection.relaying = true; // every connection, no checks
     return next(CONT);
   };
   ```

   `npFilter` sets `relaying = true` only for allowlisted IPs, but `npRoute`
   sets it for everyone. Safety currently depends entirely on `npFilter`
   returning `DENYDISCONNECT` first — i.e. on plugin order in `config/plugins`
   and on `npFilter` being enabled. If `npFilter` is disabled, reordered, or its
   config is missing, this becomes an open relay. Relaying should be decided in
   exactly one place.

2. Done: **`npFilter.hook_send_email` returns `DENY`** (`npFilter.js:93`)

   ```js
   exports.hook_send_email = function (next, hmail) { ... return next(DENY); };
   ```

   Haraka auto-registers hooks by their `hook_` name, so if `npFilter` is in
   `config/plugins` this denies all outbound sending. Looks like leftover
   scaffolding — same for `hook_queue_outbound` / `hook_rcpt` in that file.
   Remove if not intended.

3. **Attachment scanner fails _open_** (`AttachChecker.js:31,72` + `npFilterAttach.js`)

   Every error path resolves to `"allow"`. If the filter service (`/filter/md5`)
   is down, all attachments pass. For a security control, failing closed
   (`DENYSOFT`) is the safer default — at minimum make it configurable.

4. **`functions.getAddr` crashes on a null sender** (`functions.js:21`)

   ```js
   let res = addr.user + "@" + addr.host;
   ```

   Bounce messages use the null sender (`MAIL FROM:<>`). If
   `hmail.todo.mail_from` is null, `npRoute.hook_get_mx` throws. Needs a guard.

5. **Routing match is case-sensitive** (`Route.js`)

   `getCheckerFunction` does `val == param`. Domains are case-insensitive per
   RFC, so a rule `sender_domain: "Example.com"` silently won't match
   `example.com`. Normalize both sides to lowercase.

## Robustness

6. **`connection.hello.host` can throw at connect time** (`npConnection.js:38`,
   `npData`, `npQueue`)

   `hook_connect` fires before HELO/EHLO, so `connection.hello` may be undefined
   → crash. Same assumption applies to `connection.client` and
   `connection.rcpt_count`. These need optional chaining.

## Maintainability

7. Done: **Heavy duplication** — added `functions.buildConnInfo(connection)` and
   `functions.postWithLogging(payload, url, logfile)`; `npConnection`, `npData`,
   `npQueue`, `npLogDelivery` now use them and shrank to ~10-15 lines each
   (the consolidated builder also folds in the #6 optional-chaining guards).
   Covered by new tests in `tests/functions.test.js`.

8. Done: **Dead code** — removed `npConnection.hook_delivered1` (and its now-unused
   `getConfig`), dropped unused `mode` / `fs` imports and the per-file `version`
   constant (now `VERSION` in functions.js), and deleted the loose
   `ngmroute_simple.js` / `testroute.js` / `test.txt` from `plugins/`.

## Suggested order

Fix #1 and #2 first (latent open-relay / mail-blocking footguns), then the
small high-value guards #4, #5, #6 — all of which the new `tests/` suite can
lock in. #7/#8 are cleanup once behavior is pinned by tests.
