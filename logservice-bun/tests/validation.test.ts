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

    it("rejects invalid domain for host", () => {
        const result = schemaDelivery.safeParse({ ...validPayload, host: "notadomain" });
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
