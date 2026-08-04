CREATE TABLE IF NOT EXISTS `Logs` (
    `id`        INT           NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `url`       VARCHAR(2048),
    `path`      VARCHAR(255),
    `query`     VARCHAR(255),
    `src_ip`    VARCHAR(255),
    `src_port`  INT,
    `referer`   VARCHAR(255),
    `origin`    VARCHAR(255),
    `method`    VARCHAR(255),
    `user`      VARCHAR(255),
    `userAgent` VARCHAR(255),
    `createdAt` DATETIME      NOT NULL,
    `updatedAt` DATETIME      NOT NULL
);
