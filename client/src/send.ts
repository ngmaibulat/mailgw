#!/usr/bin/env bun
/**
 * Quick manual send — a Bun-native counterpart to swaks.sh.
 *
 *   bun run send.ts [to] [from]
 *   SMTP_HOST=10.0.0.1 SMTP_PORT=25 bun run send.ts test@ngm.dev me@ngm.dev
 */
import { SmtpClient } from "./smtp";

const to = process.argv[2] ?? process.env.SMTP_TO ?? "test@ngm.dev";
const from = process.argv[3] ?? process.env.SMTP_FROM ?? "me@ngm.dev";
const host = process.env.SMTP_HOST ?? "127.0.0.1";
const port = Number(process.env.SMTP_PORT ?? "25");

const client = await SmtpClient.open(host, port);
const greeting = await client.greeting();
console.log(`< ${greeting.raw}`);

const { queued, uuid } = await client.sendMail({
    from,
    to,
    subject: "Test email from Bun",
    body: "Hello,\n\nThis is a test email sent via Bun's native SMTP client.\n\nRegards,\nBun",
});
await client.quit();

console.log(client.getTranscript());
console.log(`\nResult: ${queued.code} (uuid: ${uuid ?? "n/a"})`);
process.exit(queued.code === 250 ? 0 : 1);
