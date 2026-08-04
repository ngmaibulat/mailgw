-- Which profiles and relay groups a gateway receives.
--
-- Exactly one of `profile_id` / `relay_group_id` is set, selected by `kind`:
-- 'ruleset' | 'allowlist' | 'server' point at `ConfigProfiles`, 'relaygroup'
-- points at `RelayGroups`. A gateway takes at most one profile of each of the
-- first three kinds and any number of relay groups — enforced in the webui
-- rather than by a UNIQUE key, because MySQL lets NULLs repeat in a unique
-- index and half the rows here have a NULL in whichever column is unused.
CREATE TABLE IF NOT EXISTS `GatewayAssignments` (
    `id`             INT         NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `gateway_id`     INT         NOT NULL,
    `kind`           VARCHAR(32) NOT NULL,
    `profile_id`     INT,
    `relay_group_id` INT,
    `createdAt`      DATETIME    NOT NULL,
    `updatedAt`      DATETIME    NOT NULL
);

CREATE INDEX `idx_assignments_gateway` ON `GatewayAssignments` (`gateway_id`);
