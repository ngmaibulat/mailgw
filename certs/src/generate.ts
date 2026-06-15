/**
 * Generate self-signed TLS certificates from a JSON config.
 *
 *   bun run generate                 # uses ./certs.config.json
 *   bun run generate path/to.json    # explicit config path
 *
 * For each entry in the config a self-signed key + cert pair is written to its
 * `out` directory. Generated files are gitignored; the config is committed.
 */
import forge from "node-forge";
import { mkdir } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";

interface CertSpec {
    /** Label used in log output and to identify the entry. */
    name: string;
    /** Subject/issuer CN. */
    commonName: string;
    /** Subject Alternative Names (DNS names and/or IPs). Defaults to [commonName]. */
    altNames?: string[];
    /** Output directory (relative to the certs project root, or absolute). */
    out: string;
    /** Private key filename within `out` (default "server.key"). */
    keyFile?: string;
    /** Certificate filename within `out` (default "server.crt"). */
    certFile?: string;
    /** Validity in days (default 825). */
    days?: number;
    /** RSA key size in bits (default 2048). */
    keySize?: number;
    /** Subject/issuer organization name. */
    organization?: string;
}

interface CertsConfig {
    defaults?: Partial<CertSpec>;
    certs: CertSpec[];
}

// Project root is one level up from src/.
const PROJECT_ROOT = resolve(import.meta.dir, "..");

// SANs need DNS entries (type 2) and IPs (type 7) tagged differently.
const isIp = (v: string) => /^\d{1,3}(\.\d{1,3}){3}$/.test(v) || v.includes(":");

function buildAltNames(values: string[]) {
    return values.map((v) =>
        isIp(v) ? { type: 7, ip: v } : { type: 2, value: v },
    );
}

// A serial whose leading bit is set is read as a negative integer; prefix a
// zero nibble to keep it positive.
function randomSerial(): string {
    const hex = forge.util.bytesToHex(forge.random.getBytesSync(16));
    return parseInt(hex[0]!, 16) >= 8 ? `0${hex}` : hex;
}

function generateCert(spec: Required<Pick<CertSpec, "commonName">> & CertSpec) {
    const keySize = spec.keySize ?? 2048;
    const days = spec.days ?? 825;
    const altNames = spec.altNames?.length ? spec.altNames : [spec.commonName];

    const keys = forge.pki.rsa.generateKeyPair(keySize);
    const cert = forge.pki.createCertificate();

    cert.publicKey = keys.publicKey;
    cert.serialNumber = randomSerial();

    const now = new Date();
    cert.validity.notBefore = now;
    cert.validity.notAfter = new Date(now.getTime() + days * 24 * 60 * 60 * 1000);

    const attrs = [
        { name: "commonName", value: spec.commonName },
        ...(spec.organization ? [{ name: "organizationName", value: spec.organization }] : []),
    ];
    cert.setSubject(attrs);
    cert.setIssuer(attrs); // self-signed: issuer == subject

    cert.setExtensions([
        { name: "basicConstraints", cA: false },
        { name: "keyUsage", digitalSignature: true, keyEncipherment: true },
        { name: "extKeyUsage", serverAuth: true },
        { name: "subjectAltName", altNames: buildAltNames(altNames) },
    ]);

    cert.sign(keys.privateKey, forge.md.sha256.create());

    return {
        key: forge.pki.privateKeyToPem(keys.privateKey),
        cert: forge.pki.certificateToPem(cert),
    };
}

async function main() {
    const arg = process.argv[2];
    const configPath = arg
        ? resolve(process.cwd(), arg)
        : resolve(PROJECT_ROOT, "certs.config.json");

    const config = (await Bun.file(configPath).json()) as CertsConfig;

    if (!Array.isArray(config.certs) || config.certs.length === 0) {
        throw new Error(`no "certs" entries in ${configPath}`);
    }

    for (const entry of config.certs) {
        const spec = { ...config.defaults, ...entry } as CertSpec;
        if (!spec.commonName) throw new Error(`cert "${spec.name}" is missing commonName`);
        if (!spec.out) throw new Error(`cert "${spec.name}" is missing out`);

        const { key, cert } = generateCert(spec as CertSpec & { commonName: string });

        const outDir = isAbsolute(spec.out) ? spec.out : resolve(PROJECT_ROOT, spec.out);
        await mkdir(outDir, { recursive: true });

        const keyPath = join(outDir, spec.keyFile ?? "server.key");
        const certPath = join(outDir, spec.certFile ?? "server.crt");
        await Bun.write(keyPath, key);
        await Bun.write(certPath, cert);

        console.log(`[certs] ${spec.name}: wrote ${keyPath} + ${certPath}`);
    }
}

await main();
