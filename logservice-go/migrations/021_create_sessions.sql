-- Persistent web sessions.
--
-- The webui kept sessions in a plain in-process object, so every restart or
-- deploy logged all operators out and auth broke outright with more than one
-- replica. A fleet console is exactly the thing you want to run more than one
-- of, so the store moves into the database.
--
-- `id` is the same uuid that goes into the signed `session` cookie.
CREATE TABLE IF NOT EXISTS `Sessions` (
    `id`        CHAR(36)     NOT NULL PRIMARY KEY,
    `email`     VARCHAR(255) NOT NULL,
    `expiresAt` DATETIME     NOT NULL,
    `createdAt` DATETIME     NOT NULL
);

-- The periodic sweep deletes by expiry.
CREATE INDEX `idx_sessions_expires` ON `Sessions` (`expiresAt`);
