-- Immutable per-gateway configuration bundles, plus the deploy audit trail.
--
-- Pressing Deploy composes a gateway's assigned profiles and relay groups into
-- one JSON bundle and inserts it here; nothing ever updates a row. That is what
-- makes rollback trivial: it repoints `Gateways.desired_version_id` at an older
-- row rather than minting a new bundle, so rolling back is byte-identical to
-- what ran before.
CREATE TABLE IF NOT EXISTS `ConfigVersions` (
    `id`            INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `gateway_id`    INT          NOT NULL,
    -- Per-gateway counter, 1-based. Displayed as "v3"; `id` is the join key.
    `version`       INT          NOT NULL,
    `bundle`        LONGTEXT     NOT NULL,
    -- SHA-256 of `bundle`, hex. The gateway compares this before downloading a
    -- bundle it may already hold, so the status poll stays cheap.
    `bundle_sha256` CHAR(64)     NOT NULL,
    `note`          VARCHAR(255),
    `created_by`    VARCHAR(255),
    `createdAt`     DATETIME     NOT NULL
);

CREATE UNIQUE INDEX `uniq_version_gateway` ON `ConfigVersions` (`gateway_id`, `version`);

-- Who deployed or rolled back what, and when.
CREATE TABLE IF NOT EXISTS `ConfigDeployments` (
    `id`         INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `gateway_id` INT          NOT NULL,
    `version_id` INT          NOT NULL,
    -- 'deploy' | 'rollback'
    `action`     VARCHAR(16)  NOT NULL,
    `actor`      VARCHAR(255),
    `note`       VARCHAR(255),
    `createdAt`  DATETIME     NOT NULL
);

CREATE INDEX `idx_deployments_gateway` ON `ConfigDeployments` (`gateway_id`);
