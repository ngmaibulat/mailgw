CREATE TABLE IF NOT EXISTS `Users` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `email`     VARCHAR(255) NOT NULL UNIQUE,
    `hash`      VARCHAR(255),
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
