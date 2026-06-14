CREATE TABLE IF NOT EXISTS `Transaction` (
    `id`                  INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `uuid`                VARCHAR(255),
    `dt`                  DATETIME,
    `action`              VARCHAR(255),
    `encoding`            VARCHAR(255),
    `sender`              VARCHAR(255),
    `rcpt_list`           VARCHAR(255),
    `rcpt_count_accept`   INT,
    `rcpt_count_tempfail` INT,
    `rcpt_count_reject`   INT,
    `delay_data_post`     DOUBLE,
    `data_bytes`          INT,
    `mime_part_count`     INT,
    `createdAt`           DATETIME     NOT NULL,
    `updatedAt`           DATETIME     NOT NULL
);
