/**
 * Running `mailgw-go-test` as a real OS process.
 *
 * # Why a process and not a Go test
 *
 * mailgw-go's own suite already covers rule semantics, TLS, AUTH, DSN rendering
 * and the listener chain, in-process and under -race. What it cannot cover is
 * anything that needs a process boundary: a restart over one data directory, an
 * exit code, the binary's own flags, or the gateway's SMTP client talking to
 * something that is not a Go test helper. That is what this file is for.
 *
 * # No sleeps, and no port races
 *
 * All three listeners are asked for port 0 and all three answers come back:
 *
 *   SMTP     status.listeners[]   (the bundle asked for :0)
 *   control  the "testctl <addr>" line on stdout
 *   admin    status.admin_addr
 *
 * Reserve-and-release is used nowhere. Readiness is a race between that stdout
 * line, the process exiting, and a deadline — so an unwritable data directory,
 * a taken port or a bad flag surfaces as its own exit code and stderr rather
 * than as a timeout twenty seconds later.
 */

import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { binaryPath } from "./binary.ts";
import type { Bundle } from "./bundle.ts";
import { track, untrack } from "./reap.ts";
import { SmtpClient } from "./smtp.ts";
import { Testctl, type Status } from "./testctl.ts";

/** How long the process gets to print its control address. */
const START_TIMEOUT_MS = 20_000;
/** How long SIGTERM gets before SIGKILL. The baseline sets shutdown_timeout: 2s. */
const STOP_TIMEOUT_MS = 15_000;
/** Log records kept per gateway, for the message attached to a failure. */
const LOG_RING = 2000;

export interface LogRecord {
    time?: string;
    level?: string;
    msg?: string;
    [k: string]: unknown;
}

export interface GatewayOptions {
    /** Applied once the process is up. Omit to test the unconfigured state. */
    bundle?: Bundle;
    /** Keep the data directory after stop(), for debugging. */
    keepData?: boolean;
}

export class Gateway {
    readonly ctl: Testctl;

    private stderrText = "";
    private records: LogRecord[] = [];
    private stopped = false;

    private constructor(
        private proc: Bun.Subprocess,
        readonly dataDir: string,
        readonly ctlUrl: string,
        private readonly keepData: boolean,
    ) {
        this.ctl = new Testctl(ctlUrl);
    }

    /** The bound SMTP address, or "" when nothing is listening yet. */
    async smtpAddr(): Promise<string> {
        const st = await this.ctl.status();
        return st.listeners[0] ?? "";
    }

    /** Open an SMTP session against the bound port, greeting consumed. */
    async smtp(): Promise<SmtpClient> {
        const addr = await this.smtpAddr();
        if (!addr) {
            throw new Error(this.decorate("the gateway is not listening on SMTP"));
        }
        const [host, port] = splitAddr(addr);
        const c = await SmtpClient.open(host, port);
        await c.greeting();
        return c;
    }

    /** The bound admin address, where /metrics, /readyz and /healthz live. */
    async adminUrl(): Promise<string> {
        const st = await this.ctl.status();
        if (!st.admin_addr) throw new Error(this.decorate("the admin UI is not listening"));
        return `http://${st.admin_addr}`;
    }

    /**
     * The Prometheus exposition, parsed into a flat map of sample name to value.
     *
     * The real endpoint rather than a JSON side-door: asserting on it also pins
     * the exposition format, which is the operator contract.
     */
    async metrics(token?: string): Promise<Record<string, number>> {
        const res = await fetch((await this.adminUrl()) + "/metrics", {
            headers: token ? { authorization: `Bearer ${token}` } : undefined,
            signal: AbortSignal.timeout(10_000),
        });
        if (!res.ok) throw new Error(`GET /metrics -> ${res.status}`);
        return parseExposition(await res.text());
    }

    /**
     * Wait for the status to satisfy a predicate.
     *
     * Needed because startup is not one moment. The control API answers as soon
     * as it binds, but Run boots from the cache on its own goroutine — so
     * immediately after a restart the gateway can be reachable and not yet
     * serving. Waiting for the control API and then asserting on `serving`
     * would be a race that passes on a fast machine.
     */
    async waitForStatus(
        pred: (s: Status) => boolean,
        timeoutMs = 20_000,
    ): Promise<Status> {
        const deadline = Date.now() + timeoutMs;
        let last: Status | undefined;
        for (;;) {
            try {
                last = await this.ctl.status();
                if (pred(last)) return last;
            } catch {
                // The API may still be coming up; that is what the deadline is
                // for.
            }
            if (Date.now() > deadline) {
                throw new Error(
                    this.decorate(
                        `status never matched in ${timeoutMs}ms; last = ${JSON.stringify(last)}`,
                    ),
                );
            }
            await Bun.sleep(25);
        }
    }

    /** Wait until SMTP is up, which is the usual thing to wait for. */
    waitUntilServing(timeoutMs = 20_000): Promise<Status> {
        return this.waitForStatus((s) => s.serving && s.listeners.length > 0, timeoutMs);
    }

    /** Everything the gateway has written to stderr. */
    logs(): string {
        return this.stderrText;
    }

    /** The gateway's slog output, parsed. Non-JSON lines are skipped. */
    logLines(): LogRecord[] {
        return this.records;
    }

    /** Wait for a log record, e.g. a specific warning. */
    async waitForLog(
        pred: (r: LogRecord) => boolean,
        timeoutMs = 10_000,
    ): Promise<LogRecord> {
        const deadline = Date.now() + timeoutMs;
        for (;;) {
            const hit = this.records.find(pred);
            if (hit) return hit;
            if (Date.now() > deadline) {
                throw new Error(this.decorate("timed out waiting for a log record"));
            }
            await Bun.sleep(25);
        }
    }

    /**
     * Stop and start again on the SAME data directory.
     *
     * This is the highest-value method here. Boot-from-cache, spool recovery and
     * the honesty of restart_required are all claims about two processes over
     * one directory, and nothing in the Go suite can span that.
     *
     * The SMTP port changes — it was ephemeral — so callers must re-read it.
     */
    async restart(): Promise<void> {
        await this.stop({ keepData: true });
        const started = await spawnGateway(this.dataDir, this.keepData);
        this.proc = started.proc;
        this.stderrText = "";
        this.records = [];
        this.stopped = false;
        track(this.proc);
        this.pump();
        // The control port is ephemeral too, so the client is re-pointed.
        (this.ctl as { baseUrl: string }).baseUrl = started.ctlUrl;
        (this as { ctlUrl: string }).ctlUrl = started.ctlUrl;
        await waitReady(this.ctl, () => this.stderrText);
    }

    /** SIGTERM, then SIGKILL if it overstays. Returns the exit code. */
    async stop(o: { keepData?: boolean } = {}): Promise<number> {
        if (this.stopped) return this.proc.exitCode ?? 0;
        this.stopped = true;

        this.proc.kill("SIGTERM");
        const code = await Promise.race([
            this.proc.exited,
            Bun.sleep(STOP_TIMEOUT_MS).then(() => {
                this.proc.kill("SIGKILL");
                return -1;
            }),
        ]);
        untrack(this.proc);

        if (!(o.keepData ?? this.keepData)) {
            await rm(this.dataDir, { recursive: true, force: true });
        }
        if (code === -1) {
            throw new Error(
                this.decorate("the gateway did not exit within " + STOP_TIMEOUT_MS + "ms"),
            );
        }
        return code;
    }

    /**
     * Attach the last of the gateway's log to an error message.
     *
     * Without this a Tier-B failure in CI reads as "timed out" and the reason —
     * which is in the gateway's own stderr — is thrown away.
     */
    decorate(msg: string): string {
        const tail = this.stderrText.split("\n").slice(-40).join("\n");
        return `${msg}\n  data dir: ${this.dataDir}\n  control:  ${this.ctlUrl}\n--- gateway log (last 40 lines) ---\n${tail}\n---`;
    }

    private pump(): void {
        void (async () => {
            const decoder = new TextDecoder();
            let pending = "";
            // @ts-expect-error stderr is a ReadableStream because we asked for "pipe"
            for await (const chunk of this.proc.stderr) {
                pending += decoder.decode(chunk, { stream: true });
                const lines = pending.split("\n");
                pending = lines.pop() ?? "";
                for (const line of lines) this.absorb(line);
            }
            if (pending) this.absorb(pending);
        })();
    }

    private absorb(line: string): void {
        this.stderrText += line + "\n";
        try {
            this.records.push(JSON.parse(line) as LogRecord);
            if (this.records.length > LOG_RING) this.records.shift();
        } catch {
            // Not a slog record — a panic trace, or the flag package's usage.
            // Kept in stderrText, which is what a failure message prints.
        }
    }

    static async start(o: GatewayOptions = {}): Promise<Gateway> {
        const dataDir = await mkdtemp(path.join(os.tmpdir(), "mailgw-gw-"));
        const keepData = o.keepData ?? process.env.MAILGW_KEEP_DATA === "1";

        const started = await spawnGateway(dataDir, keepData);
        const gw = new Gateway(started.proc, dataDir, started.ctlUrl, keepData);
        track(started.proc);
        gw.pump();

        await waitReady(gw.ctl, () => gw.logs());
        if (o.bundle) await gw.ctl.applyBundle(o.bundle);
        return gw;
    }
}

/** Convenience wrapper so a suite reads `await startGateway({...})`. */
export function startGateway(o: GatewayOptions = {}): Promise<Gateway> {
    return Gateway.start(o);
}

interface Started {
    proc: Bun.Subprocess;
    ctlUrl: string;
}

async function spawnGateway(dataDir: string, _keepData: boolean): Promise<Started> {
    const bin = await binaryPath();

    const proc = Bun.spawn(
        [
            bin,
            // Every listener ephemeral; every bound address discoverable.
            "-testctl",
            "127.0.0.1:0",
            // -admin "" is FATAL (node.ErrNoAdminAddr): a managed node with no
            // wizard has no way to be provisioned, so the binary refuses. Give
            // it a port even when a suite never touches it.
            "-admin",
            "127.0.0.1:0",
            // Absolute, always. The store's DSN is a file: URL and a relative
            // path becomes a URI authority.
            "-data",
            dataDir,
        ],
        { stdout: "pipe", stderr: "pipe", stdin: "ignore" },
    );

    const ctlUrl = await readControlAddress(proc);
    return { proc, ctlUrl };
}

/**
 * Read the "testctl <addr>" line, or explain why it never came.
 *
 * Racing proc.exited is what turns "an unwritable data directory" into an error
 * naming the exit code and the stderr, instead of a twenty-second timeout.
 */
async function readControlAddress(proc: Bun.Subprocess): Promise<string> {
    let out = "";
    const decoder = new TextDecoder();

    const read = (async () => {
        // @ts-expect-error stdout is a ReadableStream because we asked for "pipe"
        for await (const chunk of proc.stdout) {
            out += decoder.decode(chunk, { stream: true });
            const m = out.match(/^testctl (\S+)$/m);
            if (m) return `http://${m[1]}`;
        }
        return null;
    })();

    const died = proc.exited.then(async (code) => {
        const stderr = await new Response(proc.stderr as ReadableStream).text();
        throw new Error(
            `mailgw-go-test exited with ${code} before its control API came up.\n` +
                `--- stderr ---\n${stderr}\n---`,
        );
    });

    const timeout = Bun.sleep(START_TIMEOUT_MS).then(() => {
        proc.kill("SIGKILL");
        throw new Error(
            `mailgw-go-test printed no control address within ${START_TIMEOUT_MS}ms.\n` +
                `stdout so far: ${JSON.stringify(out)}`,
        );
    });

    const addr = await Promise.race([read, died, timeout]);
    if (!addr) throw new Error("mailgw-go-test closed stdout without a control address");
    return addr as string;
}

/** Poll the control API until it answers. Fast: the port is already known. */
async function waitReady(ctl: Testctl, logs: () => string): Promise<void> {
    const deadline = Date.now() + START_TIMEOUT_MS;
    for (;;) {
        try {
            await ctl.status();
            return;
        } catch (e) {
            if (Date.now() > deadline) {
                throw new Error(
                    `the control API at ${ctl.baseUrl} never answered: ${(e as Error).message}\n` +
                        `--- gateway log ---\n${logs()}\n---`,
                );
            }
            await Bun.sleep(20);
        }
    }
}

/** "127.0.0.1:2525" -> ["127.0.0.1", 2525], tolerating IPv6 brackets. */
export function splitAddr(addr: string): [string, number] {
    const i = addr.lastIndexOf(":");
    const host = addr.slice(0, i).replace(/^\[|\]$/g, "");
    return [host || "127.0.0.1", Number(addr.slice(i + 1))];
}

/**
 * Parse the Prometheus text format into name{labels} -> value.
 *
 * Enough for assertions, not a general parser: HELP and TYPE lines are skipped
 * and the key is kept verbatim including any labels.
 */
export function parseExposition(text: string): Record<string, number> {
    const out: Record<string, number> = {};
    for (const line of text.split("\n")) {
        if (!line || line.startsWith("#")) continue;
        const sp = line.lastIndexOf(" ");
        if (sp < 0) continue;
        const value = Number(line.slice(sp + 1));
        if (Number.isFinite(value)) out[line.slice(0, sp)] = value;
    }
    return out;
}
