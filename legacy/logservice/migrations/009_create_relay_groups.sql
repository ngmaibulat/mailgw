CREATE TABLE IF NOT EXISTS `RelayGroups` (
    `id`          INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`        VARCHAR(255),
    `description` VARCHAR(255),
    `createdAt`   DATETIME     NOT NULL,
    `updatedAt`   DATETIME     NOT NULL
);
