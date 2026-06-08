CREATE TABLE IF NOT EXISTS `HashLookups` (
    `id`          INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `txn_uuid`    VARCHAR(255),
    `md5`         VARCHAR(255),
    `contentType` VARCHAR(255),
    `filename`    VARCHAR(255),
    `size`        INT,
    `action`      VARCHAR(255),
    `createdAt`   DATETIME     NOT NULL,
    `updatedAt`   DATETIME     NOT NULL
);
