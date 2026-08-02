import crypto from "node:crypto";

// Encryption at rest for relay credentials.
//
// # What this protects, and what it does not
//
// The threat is a copy of the console's database — a backup, a dump, a replica,
// a `SELECT * FROM Relays` in a log. Before this, every relay password sat in
// that database in the clear, and `Relays` is the one table whose contents are
// somebody else's credentials.
//
// It is NOT protection against a compromised console: the console has to be
// able to read these values to compose a bundle, so anything that can run code
// there can read them too. That is inherent, not an oversight.
//
// # Why the CONSOLE decrypts, and not the gateway
//
// The alternative — ship ciphertext in the bundle and let each gateway decrypt
// — would require every gateway in the fleet to hold the key. A fleet-wide
// shared secret distributed to internet-facing relays is strictly worse than
// the problem it solves: one compromised edge node would yield every relay
// credential for every gateway, not just its own.
//
// So bundles carry plaintext, over TLS, to a gateway that already needs the
// password in memory to use it. The genuinely stronger option for anyone who
// wants it is `auth_pass_env`, which keeps the credential out of the bundle
// entirely (migration 022, mailgw-go's relays.Relay.Password()).
//
// # Format
//
//     v1:<base64( 12-byte iv || ciphertext || 16-byte tag )>
//
// The prefix is what makes this deployable against an existing install: a
// stored value without it is a pre-migration plaintext password and is returned
// as-is, so nothing breaks on upgrade and rows migrate as they are re-saved.

const PREFIX = "v1:";
const IV_BYTES = 12;
const TAG_BYTES = 16;
const KEY_BYTES = 32;

let cachedKey: Buffer | null | undefined;

// Accepts base64 or hex, because operators generate these with whichever of
// `openssl rand -base64 32` / `-hex 32` they reach for first. Anything that is
// not exactly 32 bytes is a hard error rather than a silently weaker key.
function parseKey(raw: string): Buffer {
    const trimmed = raw.trim();

    if (/^[0-9a-fA-F]{64}$/.test(trimmed)) {
        return Buffer.from(trimmed, "hex");
    }

    const decoded = Buffer.from(trimmed, "base64");
    if (decoded.length !== KEY_BYTES) {
        throw new Error(
            `CONFIG_SECRET_KEY must decode to ${KEY_BYTES} bytes ` +
                `(got ${decoded.length}); generate one with ` +
                `\`openssl rand -base64 32\``,
        );
    }
    return decoded;
}

// The key is read once and cached. Reading it lazily rather than at import time
// keeps `app.inject()` tests and `create_user.ts` runnable without it.
function key(): Buffer | null {
    if (cachedKey !== undefined) {
        return cachedKey;
    }
    const raw = process.env.CONFIG_SECRET_KEY;
    cachedKey = raw ? parseKey(raw) : null;
    return cachedKey;
}

// Test seam: the key is process-wide state, and a test that sets the env var
// after this module loaded would otherwise see the cached miss.
export function resetSecretKeyCache(): void {
    cachedKey = undefined;
}

// isEnabled reports whether credentials will actually be encrypted. The
// startup banner says so, because "I configured a key" and "the key is in
// effect" being different states is exactly how this goes wrong quietly.
export function isEnabled(): boolean {
    return key() !== null;
}

// encrypt returns the value to store. Without a key it returns the plaintext
// unchanged: refusing to save a relay because encryption is not configured
// would make this a breaking change rather than an improvement, and the
// unencrypted state is the one every existing install is already in.
export function encrypt(plain: string): string {
    if (plain === "") {
        return "";
    }
    const k = key();
    if (!k) {
        return plain;
    }

    const iv = crypto.randomBytes(IV_BYTES);
    const cipher = crypto.createCipheriv("aes-256-gcm", k, iv);
    const ct = Buffer.concat([cipher.update(plain, "utf8"), cipher.final()]);

    return (
        PREFIX + Buffer.concat([iv, ct, cipher.getAuthTag()]).toString("base64")
    );
}

// decrypt reverses encrypt. A value with no prefix predates encryption and is
// returned unchanged.
//
// A prefixed value that will not decrypt throws: it means the key is wrong or
// the row was tampered with, and silently substituting an empty password would
// send a gateway out to authenticate with nothing and fail in a way that points
// nowhere near the cause.
export function decrypt(stored: string | null | undefined): string {
    if (!stored) {
        return "";
    }
    if (!stored.startsWith(PREFIX)) {
        return stored;
    }

    const k = key();
    if (!k) {
        throw new Error(
            "a relay password is encrypted but CONFIG_SECRET_KEY is not set; " +
                "the gateway bundle cannot be composed without it",
        );
    }

    const raw = Buffer.from(stored.slice(PREFIX.length), "base64");
    if (raw.length < IV_BYTES + TAG_BYTES) {
        throw new Error("stored relay password is truncated");
    }

    const iv = raw.subarray(0, IV_BYTES);
    const tag = raw.subarray(raw.length - TAG_BYTES);
    const ct = raw.subarray(IV_BYTES, raw.length - TAG_BYTES);

    const decipher = crypto.createDecipheriv("aes-256-gcm", k, iv);
    decipher.setAuthTag(tag);
    try {
        return Buffer.concat([decipher.update(ct), decipher.final()]).toString(
            "utf8",
        );
    } catch {
        // The GCM tag check failed. Deliberately not echoing the underlying
        // error, which says nothing useful and varies by Node version.
        throw new Error(
            "a relay password could not be decrypted; " +
                "CONFIG_SECRET_KEY does not match the one it was saved with",
        );
    }
}

// isEncrypted reports whether a stored value is ciphertext, for the "leave
// blank to keep" edit path and for the upgrade banner.
export function isEncrypted(stored: string | null | undefined): boolean {
    return typeof stored === "string" && stored.startsWith(PREFIX);
}
