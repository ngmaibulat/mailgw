-- Gateway inventory for Central Management.
--
-- A mailgw-go instance generates an Ed25519 keypair on first boot and registers
-- itself: registration is open (no enrollment token), so the row lands with
-- `status = 'pending'` and an operator approves the *fingerprint* in the web UI.
-- The fingerprint is therefore the identity that matters; `gateway_uid` is only
-- a stable handle for URLs and for stamping log rows.
--
-- `desired_version_id` is what Central wants the gateway to run;
-- `applied_version_id` is what the gateway last reported actually running. The
-- two differing is normal (a deploy in flight) — a difference that persists,
-- together with `apply_error`, is what surfaces a bad config in the UI.
CREATE TABLE IF NOT EXISTS `Gateways` (
    `id`                 INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `gateway_uid`        CHAR(36)     NOT NULL UNIQUE,
    `name`               VARCHAR(255),
    -- SHA-256 of the raw 32-byte public key, hex. This is what the operator
    -- compares against the gateway's own status page before approving.
    `fingerprint`        VARCHAR(128) NOT NULL UNIQUE,
    -- base64 of the raw 32-byte Ed25519 public key.
    `pubkey`             VARCHAR(255) NOT NULL,
    `status`             VARCHAR(16)  NOT NULL DEFAULT 'pending',

    -- Systeminfo, sent at registration and refreshed on every report. Advisory
    -- only: it is self-declared by the gateway and must never gate a decision.
    `hostname`           VARCHAR(255),
    `os`                 VARCHAR(64),
    `arch`               VARCHAR(64),
    `cpus`               INT,
    `mem_bytes`          BIGINT,
    `ip_addrs`           TEXT,
    `version`            VARCHAR(64),

    `first_seen`         DATETIME,
    `last_seen`          DATETIME,
    `approved_by`        VARCHAR(255),
    `approved_at`        DATETIME,

    `desired_version_id` INT,
    `applied_version_id` INT,
    -- Set by the gateway when a pulled bundle fails to parse or type-check. The
    -- gateway keeps its last-good configuration running; this is how that shows
    -- up in the console instead of being silent.
    `apply_error`        TEXT,
    -- Only the allowlist and the rule set are hot-swappable. A bundle changing
    -- the relay table, listeners, TLS or the spool needs a process restart.
    `restart_required`   TINYINT(1)   NOT NULL DEFAULT 0,

    `createdAt`          DATETIME     NOT NULL,
    `updatedAt`          DATETIME     NOT NULL
);

CREATE INDEX `idx_gateways_status` ON `Gateways` (`status`);
