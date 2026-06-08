CREATE TABLE IF NOT EXISTS `linkAddrTransactions` (
    `id`            INT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `MailAddrId`    INT,
    `TransactionId` INT,
    `createdAt`     DATETIME NOT NULL,
    `updatedAt`     DATETIME NOT NULL
);
