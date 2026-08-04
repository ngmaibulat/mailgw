CREATE TABLE IF NOT EXISTS `Relays` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `group_id`  INT,
    `name`      VARCHAR(255),
    `host`      VARCHAR(255),
    `port`      INT,
    `auth_user` VARCHAR(255),
    `auth_pass` VARCHAR(255),
    `priority`  INT,
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
