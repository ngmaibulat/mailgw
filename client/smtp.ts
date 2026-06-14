/**
 * Minimal SMTP client built on Bun's native TCP sockets (`Bun.connect`).
 *
 * Zero external dependencies — it speaks just enough SMTP to exercise the
 * running mailgw the same way `swaks.sh` does, and is used by the Bun test
 * suite (`smtp.test.ts`) and the `send.ts` CLI.
 */

const CRLF = "\r\n";

export interface SmtpReply {
    /** Numeric status, e.g. 220, 250, 354, 550. */
    code: number;
    /** Each protocol line of the reply, e.g. ["250-mx Hello", "250 SIZE 100"]. */
    lines: string[];
    /** Full reply text (lines joined with CRLF). */
    raw: string;
}

export interface MailOptions {
    from: string;
    to: string;
    subject?: string;
    body?: string;
    helo?: string;
}

export interface SendResult {
    /** The reply to the end-of-DATA terminator (the "250 ... Queued"). */
    queued: SmtpReply;
    /** The message-queue UUID parsed from the queued reply, if present. */
    uuid: string | null;
    /** Human-readable transcript of the whole conversation. */
    transcript: string;
}

// A reply is complete when a line is "<code><space>..." (vs "<code>-..." continuation).
const isFinalLine = (line: string) => /^\d{3} /.test(line);

export class SmtpClient {
    private socket!: Awaited<ReturnType<typeof Bun.connect>>;
    private buffer = "";
    private waiter: {
        resolve: (r: SmtpReply) => void;
        reject: (e: Error) => void;
        timer: ReturnType<typeof setTimeout>;
    } | null = null;
    private fatal: Error | null = null;
    private transcript: string[] = [];

    static async open(host = "127.0.0.1", port = 25): Promise<SmtpClient> {
        const client = new SmtpClient();
        client.socket = await Bun.connect({
            hostname: host,
            port,
            socket: {
                data: (_s, data) => client.onData(data),
                error: (_s, err) => client.onFatal(err),
                close: () => client.onFatal(new Error("connection closed by peer")),
            },
        });
        return client;
    }

    private onData(data: Uint8Array) {
        this.buffer += Buffer.from(data).toString("utf8");
        this.drain();
    }

    private onFatal(err: Error) {
        this.fatal = err;
        if (this.waiter) {
            clearTimeout(this.waiter.timer);
            const { reject } = this.waiter;
            this.waiter = null;
            reject(err);
        }
    }

    // Resolve a pending read as soon as the buffer holds a complete reply.
    private drain() {
        if (!this.waiter) return;
        const lines = this.buffer.split(CRLF);
        for (let i = 0; i < lines.length - 1; i++) {
            if (!isFinalLine(lines[i])) continue;
            const replyLines = lines.slice(0, i + 1);
            this.buffer = lines.slice(i + 1).join(CRLF);
            const raw = replyLines.join(CRLF);
            this.transcript.push(raw.replace(/^/gm, "< "));
            const reply: SmtpReply = { code: Number(replyLines[i].slice(0, 3)), lines: replyLines, raw };
            const { resolve, timer } = this.waiter;
            clearTimeout(timer);
            this.waiter = null;
            resolve(reply);
            return;
        }
    }

    /** Wait for the next complete server reply. */
    read(timeoutMs = 10_000): Promise<SmtpReply> {
        if (this.fatal) return Promise.reject(this.fatal);
        if (this.waiter) return Promise.reject(new Error("a read is already in progress"));
        return new Promise<SmtpReply>((resolve, reject) => {
            const timer = setTimeout(() => {
                this.waiter = null;
                reject(new Error(`SMTP read timed out after ${timeoutMs}ms`));
            }, timeoutMs);
            this.waiter = { resolve, reject, timer };
            this.drain();
        });
    }

    /** The 220 service greeting sent by the server right after connect. */
    greeting(timeoutMs?: number): Promise<SmtpReply> {
        return this.read(timeoutMs);
    }

    /** Send one command line (CRLF appended) and return the reply. */
    async cmd(line: string, timeoutMs?: number): Promise<SmtpReply> {
        this.transcript.push(`> ${line}`);
        this.socket.write(line + CRLF);
        return this.read(timeoutMs);
    }

    /** Run the full EHLO → MAIL → RCPT → DATA exchange. */
    async sendMail(opts: MailOptions): Promise<SendResult> {
        const helo = opts.helo ?? "bun-smtp-test";
        const message =
            `From: <${opts.from}>${CRLF}` +
            `To: <${opts.to}>${CRLF}` +
            `Subject: ${opts.subject ?? "Bun SMTP test"}${CRLF}` +
            CRLF +
            (opts.body ?? "Hello from a Bun-native SMTP client.") +
            CRLF;

        expect(await this.cmd(`EHLO ${helo}`), 250, "EHLO");
        expect(await this.cmd(`MAIL FROM:<${opts.from}>`), 250, "MAIL FROM");
        expect(await this.cmd(`RCPT TO:<${opts.to}>`), 250, "RCPT TO");
        expect(await this.cmd("DATA"), 354, "DATA");
        // Test bodies never begin a line with ".", so no dot-stuffing needed.
        const queued = expect(await this.cmd(`${message}.`), 250, "end-of-DATA");

        const uuid = queued.raw.match(/\(([0-9A-F-]+)(?:\.\d+)?\)/i)?.[1] ?? null;
        return { queued, uuid, transcript: this.transcript.join("\n") };
    }

    async quit(): Promise<void> {
        try {
            await this.cmd("QUIT", 3000);
        } catch {
            // Servers often drop the socket immediately after 221; ignore.
        } finally {
            this.socket.end();
        }
    }

    getTranscript(): string {
        return this.transcript.join("\n");
    }
}

function expect(reply: SmtpReply, code: number, stage: string): SmtpReply {
    if (reply.code !== code) {
        throw new Error(`${stage}: expected ${code} but got ${reply.code} — ${reply.raw}`);
    }
    return reply;
}
