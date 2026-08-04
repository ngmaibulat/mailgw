import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { after, describe, it } from "node:test";

import { ensureTlsPair } from "../src/tls.ts";

// The console has no cleartext mode, so this function decides whether it starts
// at all. Two properties matter and both are load-bearing:
//
//   * it generates when the directory is empty — that is what lets a bare
//     `docker compose up` serve TLS with no preparatory step;
//   * it NEVER touches a pair that is already there — that is what keeps a real
//     certificate on a real deployment.

const tmpDirs: string[] = [];
function tmp(): string {
    const d = fs.mkdtempSync(path.join(os.tmpdir(), "mailgw-tls-"));
    tmpDirs.push(d);
    return d;
}

after(() => {
    for (const d of tmpDirs) fs.rmSync(d, { recursive: true, force: true });
});

describe("ensureTlsPair", () => {
    it("generates a usable self-signed pair into an empty directory", () => {
        const dir = tmp();
        const pair = ensureTlsPair(dir);

        assert.match(String(pair.key), /BEGIN (RSA )?PRIVATE KEY/);
        assert.match(String(pair.cert), /BEGIN CERTIFICATE/);
        assert.ok(fs.existsSync(path.join(dir, "server.key")));
        assert.ok(fs.existsSync(path.join(dir, "server.crt")));
    });

    it("keeps the private key unreadable to anyone else", () => {
        const dir = tmp();
        ensureTlsPair(dir);
        const mode = fs.statSync(path.join(dir, "server.key")).mode & 0o777;
        assert.equal(mode, 0o600);
    });

    it("returns a mounted pair verbatim and never rewrites it", () => {
        const dir = tmp();
        fs.writeFileSync(path.join(dir, "server.key"), "KEY-FROM-THE-OPERATOR");
        fs.writeFileSync(path.join(dir, "server.crt"), "CRT-FROM-THE-OPERATOR");

        const pair = ensureTlsPair(dir);

        assert.equal(String(pair.key), "KEY-FROM-THE-OPERATOR");
        assert.equal(String(pair.cert), "CRT-FROM-THE-OPERATOR");
        // The real assertion: a production pair survives a restart untouched.
        assert.equal(
            fs.readFileSync(path.join(dir, "server.crt"), "utf8"),
            "CRT-FROM-THE-OPERATOR",
        );
    });

    it("is stable across restarts — the second boot reuses the first pair", () => {
        const dir = tmp();
        const first = ensureTlsPair(dir);
        const second = ensureTlsPair(dir);
        // Regenerating per boot would change the fingerprint an operator just
        // approved in their browser.
        assert.equal(String(first.cert), String(second.cert));
    });

    it("refuses half a pair rather than serving a mismatched one", () => {
        const dir = tmp();
        fs.writeFileSync(path.join(dir, "server.key"), "LONELY-KEY");
        assert.throws(() => ensureTlsPair(dir), /one half of a TLS pair/);
    });
});
