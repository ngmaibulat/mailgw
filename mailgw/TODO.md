# mailgw (Haraka) TODO

Improvement items for the custom plugins (`mailgw/plugins/`). The factual
description of current behavior lives in the repo-root `CLAUDE.md`
("Haraka plugin pipeline" → "Plugin inconsistencies & gotchas").

## Plugin consistency

- [ ] **`npFilterAttach`: stop hardcoding URLs.** Read `url_conn` / `url_queue`
      and the MD5 filter URL from `logging.json` (via `this.config.get`) like the
      other logging plugins, instead of the hardcoded `http://localhost:3000`
      values — the current ones break in Docker (logservice is a separate host).
- [ ] **De-duplicate event posting.** `npFilterAttach.hook_data_post` posts
      connection (`url_conn`) and transaction (`url_queue`) events that overlap
      with `npData` and `npQueue`, risking duplicate `Connection` / `Transaction`
      rows. Decide on one owner per event and remove the overlap.
- [ ] **`npConnection`: wire it up or clean it out.** It currently only writes a
      local log file and carries a dead `isBlacklistedIP()` placeholder (always
      `false`). Either POST connection events from here (and reconsider whether
      `npData` should be the one doing it at the DATA stage) or remove the dead
      code and document it as local-logging only.
- [ ] **Naming vs. behavior.** `npData` posting *connection* info is confusing;
      consider aligning plugin/hook names with the event each one emits.

## Reliability

- [ ] **Handle post failures.** `functions.postWithLogging` / `httplog` are
      fire-and-forget — the POST is not `await`ed and failures are only logged.
      Consider awaiting, retrying, or at least surfacing failures so dropped
      events are visible.
