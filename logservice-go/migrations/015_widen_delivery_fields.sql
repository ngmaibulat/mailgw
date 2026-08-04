-- Widen the delivery audit columns and index the join/lookup keys.
--
-- `response` holds a relay's reply to end-of-DATA. VARCHAR(255) truncates a
-- long reply, and under MariaDB's strict mode it errors outright — losing the
-- whole row for an otherwise successful delivery.
--
-- `rcpt_list` is widened for the same reason. The gateway now writes one
-- recipient per row, so the extra width is headroom rather than a licence to
-- store a comma-joined list.
ALTER TABLE `Delivery`
    MODIFY COLUMN `response`  TEXT,
    MODIFY COLUMN `rcpt_list` TEXT;

-- Every search path filters on `uuid`, and the smtp e2e test looks rows up with
-- `WHERE uuid LIKE '<conn-uuid>%'`. No index existed on any table.
CREATE INDEX `idx_delivery_uuid`    ON `Delivery` (`uuid`);
CREATE INDEX `idx_delivery_dt`      ON `Delivery` (`dt`);
CREATE INDEX `idx_transaction_uuid` ON `Transaction` (`uuid`);
CREATE INDEX `idx_connection_uuid`  ON `Connection` (`uuid`);

-- The attachment blocklist is probed once per attachment with `md5 IN (...)`.
CREATE INDEX `idx_blockmd5s_md5`    ON `BlockMD5s` (`md5`);

-- HashLookups is LEFT JOINed to Transaction on txn_uuid = uuid.
CREATE INDEX `idx_hashlookups_txn`  ON `HashLookups` (`txn_uuid`);
