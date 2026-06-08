CREATE TABLE IF NOT EXISTS `MailAddrs` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`      VARCHAR(255),
    `email`     VARCHAR(255) NOT NULL UNIQUE,
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
