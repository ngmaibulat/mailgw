CREATE TABLE IF NOT EXISTS `Delivery` (
    `id`            INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `uuid`          VARCHAR(255),
    `dt`            DATETIME,
    `sender`        VARCHAR(255),
    `rcpt_list`     VARCHAR(255),
    `rcpt_domain`   VARCHAR(255),
    `rcpt_accepted` VARCHAR(255),
    `tls_forced`    INT,
    `tls`           INT,
    `auth`          INT,
    `host`          VARCHAR(255),
    `ip`            VARCHAR(255),
    `port`          INT,
    `response`      VARCHAR(255),
    `delay`         DOUBLE,
    `createdAt`     DATETIME     NOT NULL,
    `updatedAt`     DATETIME     NOT NULL
);
