/**
 * The SMTP client moved to tests/harness/smtp.ts.
 *
 * It grew what Tier B needs — per-recipient reply capture, ESMTP parameters,
 * AUTH, STARTTLS, implicit TLS and dot-stuffing — and keeping two clients would
 * have meant fixing every framing bug twice.
 *
 * This re-export exists so `tests/smtp/tests/*.ts` and `send.ts` are unchanged.
 * New tests should import from `tests/harness` directly.
 */

export {
    SmtpClient,
    buildMessage,
    UUID_SCRAPER,
    type MailOptions,
    type SendResult,
    type SmtpReply,
} from "../../harness/smtp.ts";
