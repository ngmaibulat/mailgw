import assert from "node:assert/strict";
import crypto from "node:crypto";
import { afterEach, describe, it } from "node:test";

import {
    decrypt,
    encrypt,
    isEnabled,
    isEncrypted,
    resetSecretKeyCache,
} from "./secrets.ts";

// Relay credentials are the one thing in this database that belongs to somebody
// else. These tests pin the two properties that make the encryption safe to
// deploy: it round-trips, and an install that has never set a key keeps working
// exactly as it did.

const KEY_A = crypto.randomBytes(32).toString("base64");
const KEY_B = crypto.randomBytes(32).toString("base64");

function withKey(value: string | undefined) {
    if (value === undefined) {
        delete process.env.CONFIG_SECRET_KEY;
    } else {
        process.env.CONFIG_SECRET_KEY = value;
    }
    resetSecretKeyCache();
}

afterEach(() => withKey(undefined));

describe("secrets", () => {
    it("round-trips a password", () => {
        withKey(KEY_A);
        const plain = "hunter2 — with a non-ASCII em dash";
        const stored = encrypt(plain);

        assert.ok(isEncrypted(stored), "stored value should be ciphertext");
        assert.ok(
            !stored.includes(plain),
            "the plaintext must not survive in the stored value",
        );
        assert.equal(decrypt(stored), plain);
    });

    it("produces a different ciphertext each time", () => {
        withKey(KEY_A);
        // A deterministic ciphertext would leak which relays share a password.
        assert.notEqual(encrypt("same"), encrypt("same"));
    });

    it("passes pre-migration plaintext straight through", () => {
        // The upgrade case: rows written before this existed carry no prefix.
        withKey(KEY_A);
        assert.equal(decrypt("legacy-plaintext"), "legacy-plaintext");

        // ...and it still reads without a key at all, which is the state every
        // existing install is in on the day this ships.
        withKey(undefined);
        assert.equal(decrypt("legacy-plaintext"), "legacy-plaintext");
    });

    it("is a no-op without a key, rather than a failure", () => {
        withKey(undefined);
        assert.equal(isEnabled(), false);
        assert.equal(encrypt("plain"), "plain");
        assert.equal(decrypt("plain"), "plain");
    });

    it("refuses to guess when the key is wrong", () => {
        withKey(KEY_A);
        const stored = encrypt("secret");

        withKey(KEY_B);
        // Returning "" here would send a gateway out to authenticate with an
        // empty password and fail somewhere that points nowhere near the cause.
        assert.throws(() => decrypt(stored), /does not match/);
    });

    it("refuses to read ciphertext with no key configured", () => {
        withKey(KEY_A);
        const stored = encrypt("secret");

        withKey(undefined);
        assert.throws(() => decrypt(stored), /CONFIG_SECRET_KEY is not set/);
    });

    it("detects tampering", () => {
        withKey(KEY_A);
        const stored = encrypt("secret");

        // Flip a byte in the middle of the payload; GCM's tag must catch it.
        const raw = Buffer.from(stored.slice("v1:".length), "base64");
        raw[Math.floor(raw.length / 2)] ^= 0xff;
        const tampered = `v1:${raw.toString("base64")}`;

        assert.throws(() => decrypt(tampered));
    });

    it("accepts hex and base64 keys, and rejects the wrong length", () => {
        withKey(crypto.randomBytes(32).toString("hex"));
        assert.equal(isEnabled(), true);

        withKey("too-short");
        assert.throws(() => isEnabled(), /32 bytes/);
    });

    it("treats an empty password as empty", () => {
        withKey(KEY_A);
        assert.equal(encrypt(""), "");
        assert.equal(decrypt(""), "");
        assert.equal(decrypt(null), "");
    });
});
