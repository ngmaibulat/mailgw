import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import forge from "node-forge";

// The console serves HTTP/2 over TLS and has no cleartext mode, so it needs a
// key and a certificate to start at all. Until M22 that was somebody else's
// job — `pnpm certs` in the repo, `gen-certs.sh` in the labs, a manual copy on
// a core node — and forgetting it was a crash on boot with a bare ENOENT.
//
// It now mints its own when the directory holds none, which is the decision
// the gateway already made for itself (mailgw-go's tlsx.EnsureSelfSigned): a
// node that is expected to come up unattended cannot also require an
// operator-supplied file. A MOUNTED PAIR IS NEVER TOUCHED — that is the whole
// contract, and it is what keeps a real certificate on a real deployment.
//
// A self-signed certificate authenticates nothing. It is a substitute for
// cleartext, not for a certificate, and the WARN says so every boot.
const KEY = "server.key";
const CRT = "server.crt";

// Long enough not to expire mid-lab, short enough that a forgotten one is not
// forever. Matches the certs/ project's default.
const DAYS = 825;

export interface TlsPair {
    key: string | Buffer;
    cert: string | Buffer;
}

// SANs tag DNS names (type 2) and IPs (type 7) differently.
const isIp = (v: string) =>
    /^\d{1,3}(\.\d{1,3}){3}$/.test(v) || v.includes(":");

// The names this certificate should be valid for. `localhost` and the loopback
// address cover a browser on the host; the container's own hostname covers
// another container reaching it by name, which is how a gateway reaches the
// console (`https://lab-webui:4000`). TLS_HOSTS adds more, comma-separated,
// for anything neither of those catches.
function altNames(): string[] {
    const extra = (process.env.TLS_HOSTS ?? "")
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
    return [...new Set(["localhost", "127.0.0.1", os.hostname(), ...extra])];
}

// A serial whose leading bit is set reads as a negative integer; prefix a zero
// nibble to keep it positive. Same rule as certs/src/generate.ts.
function randomSerial(): string {
    const hex = forge.util.bytesToHex(forge.random.getBytesSync(16));
    return Number.parseInt(hex[0] as string, 16) >= 8 ? `0${hex}` : hex;
}

function selfSign(names: string[]): { key: string; cert: string } {
    const keys = forge.pki.rsa.generateKeyPair(2048);
    const cert = forge.pki.createCertificate();

    cert.publicKey = keys.publicKey;
    cert.serialNumber = randomSerial();

    const now = new Date();
    cert.validity.notBefore = now;
    cert.validity.notAfter = new Date(now.getTime() + DAYS * 86_400_000);

    const attrs = [
        { name: "commonName", value: names[0] as string },
        { name: "organizationName", value: "mailgw console (self-signed)" },
    ];
    cert.setSubject(attrs);
    cert.setIssuer(attrs); // self-signed: issuer == subject

    cert.setExtensions([
        { name: "basicConstraints", cA: false },
        { name: "keyUsage", digitalSignature: true, keyEncipherment: true },
        { name: "extKeyUsage", serverAuth: true },
        {
            name: "subjectAltName",
            altNames: names.map((v) =>
                isIp(v) ? { type: 7, ip: v } : { type: 2, value: v },
            ),
        },
    ]);

    cert.sign(keys.privateKey, forge.md.sha256.create());

    return {
        key: forge.pki.privateKeyToPem(keys.privateKey),
        cert: forge.pki.certificateToPem(cert),
    };
}

/**
 * Read the TLS pair from `dir`, generating a self-signed one if it is not there.
 *
 * Returns whatever is on disk when both files exist — no inspection, no expiry
 * check, no opinion. A certificate renewed in place is picked up on the next
 * restart, and one this function did not write is never overwritten.
 */
export function ensureTlsPair(dir: string): TlsPair {
    const keyPath = path.join(dir, KEY);
    const crtPath = path.join(dir, CRT);

    if (fs.existsSync(keyPath) && fs.existsSync(crtPath)) {
        return {
            key: fs.readFileSync(keyPath),
            cert: fs.readFileSync(crtPath),
        };
    }

    // Half a pair is a mistake worth stopping on rather than papering over:
    // silently generating next to somebody's key would serve a certificate
    // that does not match it.
    if (fs.existsSync(keyPath) !== fs.existsSync(crtPath)) {
        throw new Error(
            `${dir} holds one half of a TLS pair — expected both ${KEY} and ${CRT}. ` +
                "Remove the one that is there to have a self-signed pair generated, or add the other.",
        );
    }

    const names = altNames();
    const pair = selfSign(names);

    fs.mkdirSync(dir, { recursive: true });
    // 0600 on the key: this process wrote it, and nothing else needs to read it.
    fs.writeFileSync(keyPath, pair.key, { mode: 0o600 });
    fs.writeFileSync(crtPath, pair.cert, { mode: 0o644 });

    console.warn(
        `TLS: no ${KEY}/${CRT} in ${dir} — generated a self-signed pair for ${names.join(", ")}, valid ${DAYS} days.\n` +
            "     It authenticates nothing. Fine for a lab; mount a real pair into this directory for anything else.",
    );

    return pair;
}
