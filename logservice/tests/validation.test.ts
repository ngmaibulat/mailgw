import { describe, it, expect } from "bun:test";
import { schemaDelivery } from "../src/validation/delivery";

const validPayload = {
    uuid: "abc-123",
    dt: 1717833600,
    sender: "user@example.com",
    rcpt_domain: "gmail.com",
    rcpt_list: "recipient@gmail.com",
    rcpt_accepted: "recipient@gmail.com",
    tls_forced: false,
    tls: true,
    auth: false,
    host: "smtp.gmail.com",
    ip: "74.125.0.1",
    port: "25",
    response: "250 OK",
    delay: 1.23,
};

describe("schemaDelivery", () => {
    it("accepts a valid payload", () => {
        const result = schemaDelivery.safeParse(validPayload);
        expect(result.success).toBe(true);
    });

    it("rejects missing required fields", () => {
        const { sender, ...incomplete } = validPayload;
        const result = schemaDelivery.safeParse(incomplete);
        expect(result.success).toBe(false);
    });

    it("rejects invalid email for sender", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, sender: "not-an-email" });
        expect(result.success).toBe(false);
    });

    it("rejects invalid IP", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, ip: "999.999.999.999" });
        expect(result.success).toBe(false);
    });

    // Changed deliberately: `host` used to require an FQDN, which rejected the
    // delivery event for every successful relay to a single-label host —
    // `localhost`, a container name like `dev-mailhog`, or a bare IP. Because
    // the gateway posts these events asynchronously, the 400 was invisible and
    // the audit row was simply lost.
    it("accepts a single-label relay host", () => {
        for (const host of ["localhost", "dev-mailhog", "mailhog"]) {
            const result = schemaDelivery.safeParse({ ...validPayload, host });
            expect(result.success).toBe(true);
        }
    });

    it("accepts an IP literal as host", () => {
        for (const host of ["127.0.0.1", "203.0.113.10"]) {
            const result = schemaDelivery.safeParse({ ...validPayload, host });
            expect(result.success).toBe(true);
        }
    });

    it("still rejects a malformed host", () => {
        for (const host of ["", "has space", "under_score", "-leading-dash"]) {
            const result = schemaDelivery.safeParse({ ...validPayload, host });
            expect(result.success).toBe(false);
        }
    });

    // The null sender (MAIL FROM:<>) is what every bounce and DSN uses.
    // Requiring a valid address here rejected the gateway's own DSN traffic.
    it("accepts the null sender", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, sender: "" });
        expect(result.success).toBe(true);
    });

    it("accepts IPv6 addresses", () => {
        for (const ip of ["::1", "2001:db8::1", "::ffff:127.0.0.1"]) {
            const result = schemaDelivery.safeParse({ ...validPayload, ip });
            expect(result.success).toBe(true);
        }
    });

    it("accepts an empty ip when the peer was never reached", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, ip: "" });
        expect(result.success).toBe(true);
    });

    // rcpt_list and rcpt_accepted stay single-valued: the gateway emits one
    // event per recipient rather than a comma-joined list.
    it("rejects a comma-joined recipient list", () => {
        const result = schemaDelivery.safeParse({
            ...validPayload,
            rcpt_accepted: "a@example.com,b@example.com",
        });
        expect(result.success).toBe(false);
    });

    it("rejects non-numeric port", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, port: "abc" });
        expect(result.success).toBe(false);
    });

    it("rejects non-boolean tls", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, tls: "yes" });
        expect(result.success).toBe(false);
    });

    it("rejects non-number dt", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, dt: "2024-01-01" });
        expect(result.success).toBe(false);
    });

    it("extra fields do not appear in parsed.data", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, injected: "evil" });
        expect(result.success).toBe(true);
        expect((result.data as any).injected).toBeUndefined();
    });
});
