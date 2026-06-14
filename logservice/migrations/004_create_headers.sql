CREATE TABLE IF NOT EXISTS `Headers` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `mail_id`   INT,
    `name`      VARCHAR(255),
    `value`     VARCHAR(255),
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
