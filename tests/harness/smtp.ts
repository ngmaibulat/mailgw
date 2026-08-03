/**
 * A minimal SMTP client on Bun's native TCP sockets.
 *
 * Zero dependencies, and deliberately independent of any SMTP library, so the
 * tests exercise the wire protocol rather than a library's idea of it — the
 * same reasoning `internal/smtpsrv/contract_test.go` hand-rolls its client for.
 *
 * This supersedes tests/smtp/src/smtp.ts, which re-exports it. The additions
 * are the things Tier B cannot do without: per-recipient reply capture, ESMTP
 * parameters (NOTIFY/ORCPT/RET/ENVID), AUTH, STARTTLS and implicit TLS, and a
 * raw DATA path with dot-stuffing.
 */

const CRLF = "\r\n";

export interface SmtpReply {
    /** Numeric status, e.g. 220, 250, 354, 550. */
    code: number;
    /** Each protocol line, e.g. ["250-mx Hello", "250 SIZE 100"]. */
    lines: string[];
    /** Full reply text (lines joined with CRLF). */
    raw: string;
}

export interface MailOptions {
    from: string;
    /** One address, or several — each gets its own RCPT TO. */
    to: string | string[];
    subject?: string;
    body?: string;
    helo?: string;
    /** Extra headers, as raw "Name: value" lines. */
    headers?: string[];
    /** ESMTP parameters appended to MAIL FROM, e.g. ["RET=FULL"]. */
    mailParams?: string[];
    /** ESMTP parameters per recipient, e.g. {"a@x": ["NOTIFY=NEVER"]}. */
    rcptParams?: Record<string, string[]>;
}

export interface SendResult {
    /** The reply to the end-of-DATA terminator (the "250 ... Queued"). */
    queued: SmtpReply;
    /** The message-queue UUID parsed from the queued reply, if present. */
    uuid: string | null;
    /** Every RCPT TO reply, in order, so a partial rejection is visible. */
    rcptReplies: { addr: string; reply: SmtpReply }[];
    /** Human-readable transcript of the whole conversation. */
    transcript: string;
}

// A reply is complete when a line is "<code><space>..." (vs "<code>-..." continuation).
const isFinalLine = (line: string) => /^\d{3} /.test(line);

/**
 * The transaction id the gateway embeds in its 250.
 *
 * Mirrors internal/smtpsrv's uuidScraper and contract_test.go. The gateway
 * returns a deliberate 250 SMTPError to control this text; the id in
 * parentheses is a hard contract.
 */
export const UUID_SCRAPER = /\(([0-9A-F-]+)(?:\.\d+)?\)/i;

export class SmtpClient {
    private socket!: Bun.Socket<undefined>;
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
        client.socket = (await Bun.connect({
            hostname: host,
            port,
            socket: {
                data: (_s, data) => client.onData(data),
                error: (_s, err) => client.onFatal(err),
                close: () => client.onFatal(new Error("connection closed by peer")),
            },
        })) as Bun.Socket<undefined>;
        return client;
    }

    /** Connect with TLS from the first byte — an `implicit_tls` listener. */
    static async openTLS(host = "127.0.0.1", port = 465): Promise<SmtpClient> {
        const client = new SmtpClient();
        client.socket = (await Bun.connect({
            hostname: host,
            port,
            // The gateway generates a self-signed pair when its configuration
            // names none, so verification has to be off. That is the same trade
            // opportunistic TLS makes: encryption without authentication.
            tls: { rejectUnauthorized: false },
            socket: {
                data: (_s, data) => client.onData(data),
                error: (_s, err) => client.onFatal(err),
                close: () => client.onFatal(new Error("connection closed by peer")),
            },
        })) as Bun.Socket<undefined>;
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
            const reply: SmtpReply = {
                code: Number(replyLines[i].slice(0, 3)),
                lines: replyLines,
                raw,
            };
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

    /** Write without waiting — for pipelining and for the DATA body. */
    write(text: string): void {
        this.socket.write(text);
    }

    /** EHLO, returning the reply so a test can read the capability list. */
    ehlo(helo = "bun-smtp-test"): Promise<SmtpReply> {
        return this.cmd(`EHLO ${helo}`);
    }

    /** True when EHLO advertised a capability, e.g. "STARTTLS" or "AUTH". */
    static advertises(ehlo: SmtpReply, cap: string): boolean {
        return ehlo.lines.some((l) => new RegExp(`^\\d{3}[- ]${cap}\\b`, "i").test(l));
    }

    mail(from: string, params: string[] = []): Promise<SmtpReply> {
        return this.cmd(`MAIL FROM:<${from}>${params.map((p) => " " + p).join("")}`);
    }

    rcpt(to: string, params: string[] = []): Promise<SmtpReply> {
        return this.cmd(`RCPT TO:<${to}>${params.map((p) => " " + p).join("")}`);
    }

    /**
     * Upgrade an established session to TLS.
     *
     * Bun hands back a NEW socket for the encrypted side, so the handlers are
     * re-bound onto it; the old one must not be read from again.
     */
    async starttls(): Promise<SmtpReply> {
        const reply = await this.cmd("STARTTLS");
        if (reply.code !== 220) return reply;

        const [, tls] = this.socket.upgradeTLS({
            // Self-signed by design; see openTLS above.
            tls: { rejectUnauthorized: false },
            data: {
                data: (_s: unknown, data: Uint8Array) => this.onData(data),
                error: (_s: unknown, err: Error) => this.onFatal(err),
                close: () => this.onFatal(new Error("connection closed by peer")),
            },
        } as never) as [Bun.Socket<undefined>, Bun.Socket<undefined>];
        this.socket = tls;
        // The buffer belongs to the cleartext session; anything left in it is
        // not part of the encrypted conversation.
        this.buffer = "";
        return reply;
    }

    /** AUTH PLAIN, base64 of "\0user\0pass". */
    authPlain(user: string, pass: string): Promise<SmtpReply> {
        const token = Buffer.from(`\0${user}\0${pass}`, "utf8").toString("base64");
        return this.cmd(`AUTH PLAIN ${token}`);
    }

    /** AUTH LOGIN, the three-step exchange. */
    async authLogin(user: string, pass: string): Promise<SmtpReply> {
        const start = await this.cmd("AUTH LOGIN");
        if (start.code !== 334) return start;
        const afterUser = await this.cmd(Buffer.from(user, "utf8").toString("base64"));
        if (afterUser.code !== 334) return afterUser;
        return this.cmd(Buffer.from(pass, "utf8").toString("base64"));
    }

    /** Run the full EHLO → MAIL → RCPT(s) → DATA exchange. */
    async sendMail(opts: MailOptions): Promise<SendResult> {
        const helo = opts.helo ?? "bun-smtp-test";
        const rcpts = Array.isArray(opts.to) ? opts.to : [opts.to];

        expect(await this.ehlo(helo), 250, "EHLO");
        expect(await this.mail(opts.from, opts.mailParams ?? []), 250, "MAIL FROM");

        const rcptReplies: SendResult["rcptReplies"] = [];
        let accepted = 0;
        for (const addr of rcpts) {
            const reply = await this.rcpt(addr, opts.rcptParams?.[addr] ?? []);
            rcptReplies.push({ addr, reply });
            if (reply.code >= 200 && reply.code < 300) accepted++;
        }
        if (accepted === 0) {
            throw new Error(
                `every recipient was refused: ${rcptReplies.map((r) => r.reply.raw).join(" | ")}`,
            );
        }

        expect(await this.cmd("DATA"), 354, "DATA");
        const queued = await this.dataBody(buildMessage(opts, rcpts));

        return {
            queued,
            uuid: queued.raw.match(UUID_SCRAPER)?.[1] ?? null,
            rcptReplies,
            transcript: this.transcript.join("\n"),
        };
    }

    /**
     * Send a message body and the terminating dot, dot-stuffing as it goes.
     *
     * The old client skipped stuffing with a note that test bodies never begin a
     * line with ".". They do now: a body containing a bare dot is one of the
     * things worth checking survives the spool intact.
     */
    async dataBody(message: string, timeoutMs = 30_000): Promise<SmtpReply> {
        const stuffed = message.replace(/^\./gm, "..");
        const terminated = stuffed.endsWith(CRLF) ? stuffed : stuffed + CRLF;
        this.transcript.push(`> [${terminated.length} bytes of message]`);
        this.socket.write(terminated + "." + CRLF);
        return this.read(timeoutMs);
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

    /** Close without a QUIT, for testing what an abandoned session does. */
    close(): void {
        this.socket.end();
    }

    getTranscript(): string {
        return this.transcript.join("\n");
    }
}

/** Assemble a message, CRLF throughout, headers then a blank line then body. */
export function buildMessage(opts: MailOptions, rcpts: string[]): string {
    const headers = [
        `From: <${opts.from}>`,
        `To: ${rcpts.map((r) => `<${r}>`).join(", ")}`,
        `Subject: ${opts.subject ?? "Bun SMTP test"}`,
        ...(opts.headers ?? []),
    ];
    return (
        headers.join(CRLF) +
        CRLF +
        CRLF +
        (opts.body ?? "Hello from a Bun-native SMTP client.") +
        CRLF
    );
}

function expect(reply: SmtpReply, code: number, stage: string): SmtpReply {
    if (reply.code !== code) {
        throw new Error(`${stage}: expected ${code} but got ${reply.code} — ${reply.raw}`);
    }
    return reply;
}
