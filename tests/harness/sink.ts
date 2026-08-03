/**
 * A scriptable SMTP relay for the gateway to deliver to.
 *
 * # Why this exists
 *
 * MailHog accepts everything. TP-07's own preconditions say so — "A second
 * MailHog will not do this — use a small nc-scripted listener" — and every
 * interesting delivery-path behaviour needs a relay that can refuse: a 5xx that
 * bounces, a 4xx that does not, a per-recipient rejection, a connection that
 * hangs, one that is simply not there.
 *
 * It is not an MTA. It speaks exactly enough SMTP to be a believable next hop,
 * records what it was given, and answers whatever the script says.
 *
 * Two implementation details are load-bearing:
 *
 *   - Commands are buffered and split on CRLF. Assuming one command per TCP
 *     segment works until it does not, and then it is a flaky test nobody can
 *     reproduce.
 *   - Dot-stuffing is undone and \r\n.\r\n is detected properly, because half
 *     the assertions here are about the body that arrived.
 */

import type { Relay } from "./bundle.ts";

const CRLF = "\r\n";

/** A reply, or a function of the command argument, or a delay, or a hangup. */
export type Reply =
    | string
    | ((arg: string, session: SinkSession) => string)
    | { delayMs: number; reply: string }
    | "drop";

export interface SinkScript {
    greeting?: Reply;
    ehlo?: Reply;
    helo?: Reply;
    mail?: Reply;
    rcpt?: Reply;
    /** The reply to DATA itself — the 354. */
    data?: Reply;
    /** The reply to the terminating dot. The interesting one. */
    dataEnd?: Reply;
    rset?: Reply;
    quit?: Reply;
    starttls?: Reply;
    auth?: Reply;
}

export interface SinkMessage {
    from: string;
    rcpts: string[];
    /** The message, dot-unstuffed, CRLF preserved. */
    data: string;
    headers: Record<string, string[]>;
    sessionId: number;
    receivedAt: number;
}

export interface SinkSession {
    id: number;
    /** Every command line, in order. */
    commands: string[];
    from: string;
    rcpts: string[];
}

const DEFAULT_EHLO = ["250-sink.test", "250-SIZE 26214400", "250-8BITMIME", "250 PIPELINING"].join(
    CRLF,
);

export class Sink {
    readonly messages: SinkMessage[] = [];
    readonly sessions: SinkSession[] = [];

    private server: Bun.TCPSocketListener<SinkState> | null = null;
    private script: SinkScript;
    private nextId = 1;
    private open = new Set<Bun.Socket<SinkState>>();
    private failNextCount = 0;
    private failNextReply = "";

    private constructor(
        readonly host: string,
        readonly port: number,
        script: SinkScript,
        private readonly listen: () => Bun.TCPSocketListener<SinkState>,
    ) {
        this.script = script;
    }

    /** Replace the script mid-run — a relay that recovers, or starts failing. */
    setScript(next: SinkScript): void {
        this.script = next;
    }

    /** Fail the next n end-of-DATA replies, then go back to the script. */
    failNext(n: number, reply: string): void {
        this.failNextCount = n;
        this.failNextReply = reply;
    }

    /** Stop listening and cut open sockets: the relay is down (ECONNREFUSED). */
    down(): void {
        this.server?.stop(true);
        this.server = null;
        for (const s of this.open) s.end();
        this.open.clear();
    }

    /** Start listening again on the same port. */
    up(): void {
        if (!this.server) this.server = this.listen();
    }

    /**
     * Accept connections and never say anything.
     *
     * Different from down() on purpose: down() is a connect FAILURE and this is
     * a connect TIMEOUT, and they are different code paths in internal/deliver.
     * A test that conflated them would prove neither.
     */
    blackhole(): void {
        this.setScript({ greeting: { delayMs: 600_000, reply: "220 never" } });
    }

    async waitForMessage(
        pred: (m: SinkMessage) => boolean = () => true,
        timeoutMs = 20_000,
    ): Promise<SinkMessage> {
        const deadline = Date.now() + timeoutMs;
        for (;;) {
            const hit = this.messages.find(pred);
            if (hit) return hit;
            if (Date.now() > deadline) {
                throw new Error(
                    `sink at ${this.host}:${this.port} received no matching message in ${timeoutMs}ms ` +
                        `(${this.messages.length} received, ${this.sessions.length} sessions)`,
                );
            }
            await Bun.sleep(25);
        }
    }

    async waitForMessages(n: number, timeoutMs = 20_000): Promise<SinkMessage[]> {
        const deadline = Date.now() + timeoutMs;
        while (this.messages.length < n) {
            if (Date.now() > deadline) {
                throw new Error(
                    `sink at ${this.host}:${this.port} received ${this.messages.length} of ${n} messages`,
                );
            }
            await Bun.sleep(25);
        }
        return this.messages.slice(0, n);
    }

    /** Assert nothing arrives — for "a 4xx must not bounce" and friends. */
    async expectNothing(ms = 1500): Promise<void> {
        const before = this.messages.length;
        await Bun.sleep(ms);
        if (this.messages.length !== before) {
            throw new Error(
                `sink received ${this.messages.length - before} unexpected message(s): ` +
                    this.messages
                        .slice(before)
                        .map((m) => m.rcpts.join(","))
                        .join(" | "),
            );
        }
    }

    reset(): void {
        this.messages.length = 0;
        this.sessions.length = 0;
        this.failNextCount = 0;
    }

    stop(): void {
        this.down();
    }

    /** The bundle fragment for this sink, so no test hand-writes a relay. */
    relays(group = "Outbound", extra: Partial<Relay> = {}): Record<string, Relay[]> {
        return {
            [group]: [
                { name: `${group}-sink`, exchange: this.host, port: this.port, priority: 0, ...extra },
            ],
        };
    }

    static async start(script: SinkScript = {}): Promise<Sink> {
        // Port 0, then read what the kernel chose — the same rule the gateway's
        // own listeners follow, and the reason nothing here reserves a port.
        let sink!: Sink;
        const listen = () =>
            Bun.listen<SinkState>({
                hostname: "127.0.0.1",
                port: sink ? sink.port : 0,
                socket: {
                    open: (s) => sink.onOpen(s),
                    data: (s, d) => sink.onData(s, d),
                    close: (s) => sink.onClose(s),
                    error: (s) => sink.onClose(s),
                },
            });

        const server = Bun.listen<SinkState>({
            hostname: "127.0.0.1",
            port: 0,
            socket: {
                open: (s) => sink.onOpen(s),
                data: (s, d) => sink.onData(s, d),
                close: (s) => sink.onClose(s),
                error: (s) => sink.onClose(s),
            },
        });

        sink = new Sink("127.0.0.1", server.port, script, listen);
        sink.server = server;
        return sink;
    }

    private onOpen(s: Bun.Socket<SinkState>): void {
        const session: SinkSession = { id: this.nextId++, commands: [], from: "", rcpts: [] };
        this.sessions.push(session);
        s.data = { session, buffer: "", inData: false, body: "" };
        this.open.add(s);
        void this.reply(s, this.script.greeting ?? "220 sink.test ESMTP", "");
    }

    private onClose(s: Bun.Socket<SinkState>): void {
        this.open.delete(s);
    }

    private onData(s: Bun.Socket<SinkState>, chunk: Uint8Array): void {
        const st = s.data;
        st.buffer += Buffer.from(chunk).toString("utf8");

        if (st.inData) {
            // The terminator may straddle chunks, so the whole accumulated body
            // is searched rather than the newest piece.
            st.body += st.buffer;
            st.buffer = "";
            const end = st.body.indexOf(CRLF + "." + CRLF);
            if (end < 0) return;

            const raw = st.body.slice(0, end + CRLF.length);
            st.body = "";
            st.inData = false;

            const data = unstuff(raw);
            this.messages.push({
                from: st.session.from,
                rcpts: [...st.session.rcpts],
                data,
                headers: parseHeaders(data),
                sessionId: st.session.id,
                receivedAt: Date.now(),
            });

            let reply: Reply = this.script.dataEnd ?? "250 2.0.0 Ok: queued as FAKE";
            if (this.failNextCount > 0) {
                this.failNextCount--;
                reply = this.failNextReply;
            }
            void this.reply(s, reply, data);
            return;
        }

        for (;;) {
            const nl = st.buffer.indexOf(CRLF);
            if (nl < 0) break;
            const line = st.buffer.slice(0, nl);
            st.buffer = st.buffer.slice(nl + CRLF.length);
            this.command(s, line);
            if (st.inData) break;
        }
    }

    private command(s: Bun.Socket<SinkState>, line: string): void {
        const st = s.data;
        st.session.commands.push(line);

        const verb = line.split(/[ :]/, 1)[0].toUpperCase();
        const arg = line.slice(verb.length).replace(/^[: ]+/, "");

        switch (verb) {
            case "EHLO":
                return void this.reply(s, this.script.ehlo ?? DEFAULT_EHLO, arg);
            case "HELO":
                return void this.reply(s, this.script.helo ?? "250 sink.test", arg);
            case "STARTTLS":
                // Refused by default: outbound TLS is internal/deliver's own
                // suite's business, and a fake needing a certificate would be a
                // fake needing a certificate authority.
                return void this.reply(s, this.script.starttls ?? "502 5.5.1 not supported", arg);
            case "AUTH":
                return void this.reply(s, this.script.auth ?? "235 2.7.0 Authenticated", arg);
            case "MAIL": {
                st.session.from = addressIn(arg);
                st.session.rcpts = [];
                return void this.reply(s, this.script.mail ?? "250 2.1.0 Sender ok", arg);
            }
            case "RCPT": {
                const addr = addressIn(arg);
                const reply = resolve(this.script.rcpt ?? "250 2.1.5 Recipient ok", addr, st.session);
                // Only an accepted recipient joins the envelope, so a partial
                // rejection shows up in what the message says it was for.
                if (/^2/.test(reply)) st.session.rcpts.push(addr);
                return void this.write(s, reply);
            }
            case "DATA": {
                const reply = resolve(this.script.data ?? "354 End data with <CR><LF>.<CR><LF>", arg, st.session);
                if (/^3/.test(reply)) {
                    st.inData = true;
                    st.body = st.buffer;
                    st.buffer = "";
                }
                return void this.write(s, reply);
            }
            case "RSET":
                st.session.from = "";
                st.session.rcpts = [];
                return void this.reply(s, this.script.rset ?? "250 2.0.0 Ok", arg);
            case "NOOP":
                return void this.write(s, "250 2.0.0 Ok");
            case "QUIT":
                void this.reply(s, this.script.quit ?? "221 2.0.0 Bye", arg);
                return void s.end();
            default:
                return void this.write(s, "500 5.5.2 Unrecognized command");
        }
    }

    private async reply(s: Bun.Socket<SinkState>, r: Reply, arg: string): Promise<void> {
        if (r === "drop") return void s.end();
        if (typeof r === "object") {
            await Bun.sleep(r.delayMs);
            if (!this.open.has(s)) return;
            return void this.write(s, r.reply);
        }
        this.write(s, resolve(r, arg, s.data.session));
    }

    private write(s: Bun.Socket<SinkState>, line: string): void {
        try {
            s.write(line + CRLF);
        } catch {
            // The gateway hung up mid-reply, which several tests arrange.
        }
    }
}

interface SinkState {
    session: SinkSession;
    buffer: string;
    inData: boolean;
    body: string;
}

export function startSink(script: SinkScript = {}): Promise<Sink> {
    return Sink.start(script);
}

function resolve(r: Reply, arg: string, session: SinkSession): string {
    if (typeof r === "function") return r(arg, session);
    if (typeof r === "object") return r.reply;
    if (r === "drop") return "421 4.3.0 closing";
    return r;
}

/** "<a@b.test> SIZE=10" -> "a@b.test"; a bare address works too. */
function addressIn(arg: string): string {
    const m = arg.match(/<([^>]*)>/);
    if (m) return m[1];
    return arg.split(/\s+/)[0] ?? "";
}

/** Undo dot-stuffing: a line beginning ".." had a dot added by the sender. */
function unstuff(raw: string): string {
    return raw.replace(/^\.\./gm, ".");
}

/** Parse the header block into name -> values, preserving duplicates. */
export function parseHeaders(message: string): Record<string, string[]> {
    const end = message.indexOf(CRLF + CRLF);
    const block = end < 0 ? message : message.slice(0, end);

    const out: Record<string, string[]> = {};
    // Unfold continuation lines before splitting, or a folded Received: header
    // becomes several nonsense entries.
    for (const line of block.replace(/\r\n[ \t]+/g, " ").split(CRLF)) {
        const colon = line.indexOf(":");
        if (colon < 1) continue;
        const name = line.slice(0, colon).toLowerCase();
        (out[name] ??= []).push(line.slice(colon + 1).trim());
    }
    return out;
}
