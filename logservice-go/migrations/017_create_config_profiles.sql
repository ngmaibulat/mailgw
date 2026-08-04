-- Reusable configuration blocks, assigned to gateways.
--
-- `body` is the raw file text the gateway already understands — a `routing.yaml`
-- rule set, an `ngmfilter.json` allowlist, or a `server.yaml`. Storing the text
-- verbatim rather than a normalised relational model is deliberate: the rule DSL
-- is compiled and validated by Go (`internal/ruleset`), and re-implementing that
-- validation here would give two sources of truth that could disagree. The webui
-- checks shape only; the gateway is authoritative and reports `apply_error`.
--
-- Relay definitions are NOT profiles — they keep using the existing
-- `RelayGroups` / `Relays` tables, which until now had no consumer at all.
CREATE TABLE IF NOT EXISTS `ConfigProfiles` (
    `id`          INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    -- 'ruleset' | 'allowlist' | 'server'
    `kind`        VARCHAR(32)  NOT NULL,
    `name`        VARCHAR(255) NOT NULL,
    `description` VARCHAR(255),
    `body`        LONGTEXT     NOT NULL,
    `createdAt`   DATETIME     NOT NULL,
    `updatedAt`   DATETIME     NOT NULL
);

CREATE UNIQUE INDEX `uniq_profile_kind_name` ON `ConfigProfiles` (`kind`, `name`);
