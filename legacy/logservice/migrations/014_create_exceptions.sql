CREATE TABLE IF NOT EXISTS `Exceptions` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `product`   VARCHAR(255),
    `component` VARCHAR(255),
    `info`      TEXT,
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
