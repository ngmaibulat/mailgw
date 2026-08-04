-- Everything a centrally-managed gateway runs on must be expressible here.
--
-- mailgw-go nodes are now zero-config: no environment variables, no CLI flags,
-- no files on the host. Whatever the console cannot describe, a gateway cannot
-- be told — so the relay transport settings mailgw-go has always understood
-- (internal/relays/relays.go) need columns of their own.

-- Outbound TLS policy for this relay leg: none | opportunistic | required.
-- NULL/'' means opportunistic, which is what mailgw-go's Relay.TLSPolicy()
-- already defaults to, so existing rows keep behaving exactly as before.
-- Without this column a managed gateway can never REQUIRE TLS to a relay.
ALTER TABLE `Relays`
    ADD COLUMN `tls` VARCHAR(16) NULL;

-- Permit AUTH over an unencrypted connection. Off by default, deliberately:
-- this sends the relay password in the clear and should be a decision, not an
-- inherited setting.
ALTER TABLE `Relays`
    ADD COLUMN `allow_insecure_auth` TINYINT(1) NOT NULL DEFAULT 0;

-- Name of an environment variable on the GATEWAY holding the password, instead
-- of the password itself. Keeps the credential out of the deployed bundle
-- entirely — the strongest option available, and the reason `auth_pass`
-- encryption below is about protecting the console's database rather than the
-- gateway's cache.
ALTER TABLE `Relays`
    ADD COLUMN `auth_pass_env` VARCHAR(255) NULL;

-- auth_pass now stores AES-256-GCM ciphertext (see src/central/secrets.ts),
-- which is base64 and prefixed, so it no longer fits 255 characters. Values
-- written before this migration stay plaintext and are read as such; they are
-- re-encrypted the next time the relay is saved.
ALTER TABLE `Relays`
    MODIFY COLUMN `auth_pass` TEXT NULL;
