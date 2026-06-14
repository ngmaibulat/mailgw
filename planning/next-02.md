# Haraka plugin review — remaining work

From the 2026-06-14 plugin review. Items #2, #4, #6, #7, #8 are done
(refactor + robustness/null-sender guards); what's left:

## Security / correctness

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

2. **Attachment scanner fails _open_** (`AttachChecker.js:31,72` + `npFilterAttach.js`)

   Every error path resolves to `"allow"`. If the filter service (`/filter/md5`)
   is down, all attachments pass. For a security control, failing closed
   (`DENYSOFT`) is the safer default — at minimum make it configurable.

3. **Routing match is case-sensitive** (`Route.js`)

   `getCheckerFunction` does `val == param`. Domains are case-insensitive per
   RFC, so a rule `sender_domain: "Example.com"` silently won't match
   `example.com`. Normalize both sides to lowercase.
