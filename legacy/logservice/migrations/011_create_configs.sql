CREATE TABLE IF NOT EXISTS `Configs` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`      VARCHAR(255),
    `value`     VARCHAR(255),
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
