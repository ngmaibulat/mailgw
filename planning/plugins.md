# Haraka plugin review — remaining work

From the 2026-06-14 plugin review. The relaying allowlist issue,
case-insensitive routing, and items #2, #4, #6, #7, #8 are done; what's left:

## Security / correctness

1. **Attachment scanner fails _open_** (`AttachChecker.js:31,72` + `npFilterAttach.js`)

   Every error path resolves to `"allow"`. If the filter service (`/filter/md5`)
   is down, all attachments pass. For a security control, failing closed
   (`DENYSOFT`) is the safer default — at minimum make it configurable.
