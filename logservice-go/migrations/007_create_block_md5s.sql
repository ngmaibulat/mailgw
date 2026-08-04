CREATE TABLE IF NOT EXISTS `BlockMD5s` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `md5`       VARCHAR(255),
    `comment`   VARCHAR(255),
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);
