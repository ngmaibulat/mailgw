-- Inbound SMTP AUTH credentials, assigned to gateways as a set.
--
-- mailgw-go grew inbound AUTH (M13): a gateway can now advertise AUTH PLAIN and
-- AUTH LOGIN and check a submitted password, which makes submission-with-
-- credentials possible where the IP allowlist used to be the only inbound gate.
-- A zero-configuration gateway has no files and no environment of its own, so a
-- credential it should accept has to be describable here or it is unreachable in
-- practice — the same reasoning as migrations 022 and 025.
--
-- These are HASHES, and that is the one place this feature is better off than
-- `Relays.auth_pass`. A relay credential is one this console PRESENTS to
-- somebody else, so it has to be reversible and `src/central/secrets.ts`
-- encrypts it with CONFIG_SECRET_KEY. An inbound credential is only ever
-- VERIFIED, so nothing here needs to be recoverable: the console bcrypts on
-- save, the bundle carries the hash, and there is no key anywhere for a leaked
-- bundle to be decrypted with. secrets.ts is deliberately not involved.
--
-- VARCHAR(255), not TEXT: a bcrypt `$2b$…` string is 60 characters.
-- `Relays.auth_pass` needed TEXT in migration 022 only because base64 AES-GCM
-- ciphertext overflows 255.
--
-- Two tables rather than one for the same reason RelayGroups/Relays are two: a
-- gateway is assigned a SET, and a credential issued to an application server
-- that submits through three edge nodes is written once and assigned three
-- times. No foreign keys, matching every other table here — referential
-- integrity is enforced in the application (see migration 018's note on why).
CREATE TABLE IF NOT EXISTS `CredentialSets` (
    `id`          INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `name`        VARCHAR(255) NOT NULL,
    `description` VARCHAR(255),
    `createdAt`   DATETIME     NOT NULL,
    `updatedAt`   DATETIME     NOT NULL
);

CREATE UNIQUE INDEX `uniq_credential_set_name` ON `CredentialSets` (`name`);

CREATE TABLE IF NOT EXISTS `SmtpCredentials` (
    `id`        INT          NOT NULL AUTO_INCREMENT PRIMARY KEY,
    `set_id`    INT          NOT NULL,
    -- The AUTH username, compared case-sensitively by the gateway: a submission
    -- credential is an opaque string an operator issued, not a mailbox.
    `username`  VARCHAR(255) NOT NULL,
    -- bcrypt, cost 10, written by src/controllers/CtrlCredential.ts. Never a
    -- password: the gateway refuses to apply a bundle carrying anything
    -- bcrypt cannot parse, so a password pasted here fails the deploy rather
    -- than becoming a credential that silently never works.
    `hash`      VARCHAR(255) NOT NULL,
    `createdAt` DATETIME     NOT NULL,
    `updatedAt` DATETIME     NOT NULL
);

-- The gateway's own uniqueness rule, enforced here so a duplicate is refused at
-- the point somebody types it rather than at deploy time on every node.
CREATE UNIQUE INDEX `uniq_credential_set_username` ON `SmtpCredentials` (`set_id`, `username`);

-- Which set a gateway gets. NULL on every existing row, and the bundle omits
-- the `auth` key entirely when a gateway has no set assigned — so no deployed
-- configuration changes shape or digest, and nothing in the fleet re-pulls.
ALTER TABLE `GatewayAssignments`
    ADD COLUMN `credential_set_id` INT NULL;
