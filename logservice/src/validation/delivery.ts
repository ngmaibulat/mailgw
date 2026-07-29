import { z } from "zod";

const emailSchema = z.string().email();

/**
 * A relay host: a DNS name (FQDN or single label) or an IP literal.
 *
 * This deliberately accepts single-label names. Real relay targets include
 * `localhost`, container names like `dev-mailhog`, and bare IPs — an
 * FQDN-only rule rejects the delivery event for a perfectly successful
 * delivery, and because the gateway posts these events asynchronously the
 * rejection is invisible.
 */
const hostSchema = z
    .string()
    .min(1)
    .max(255)
    .regex(
        /^(?:\[?[0-9a-fA-F:.]+\]?|[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*)$/,
        "must be a hostname or IP literal",
    );

/** A recipient domain: same rules as a relay host. */
const domainSchema = hostSchema;

const ipv4 =
    /^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/;
/**
 * IPv6, including the IPv4-mapped form (::ffff:127.0.0.1). Kept intentionally
 * permissive: this is an audit field, and rejecting a well-formed address we
 * failed to anticipate would discard the record entirely.
 */
const ipv6 = /^[0-9a-fA-F:]+(?::(?:\d{1,3}\.){3}\d{1,3})?$/;

/**
 * The peer address. Empty is allowed: a delivery can fail before a peer
 * address is known, and that outcome still needs to be recorded.
 */
const ipSchema = z
    .string()
    .refine((v) => v === "" || ipv4.test(v) || (v.includes(":") && ipv6.test(v)), {
        message: "must be an IPv4 or IPv6 address",
    });

const portSchema = z.string().regex(/^[0-9]+$/);

/**
 * The envelope sender. Empty is the null sender (MAIL FROM:<>), which every
 * bounce and delivery-status notification uses — requiring a valid address
 * here rejects the gateway's own DSN traffic.
 */
const senderSchema = z.union([z.literal(""), emailSchema]);

export const schemaDelivery = z.object({
    uuid: z.string(),
    dt: z.number(),
    sender: senderSchema,
    rcpt_domain: domainSchema,
    // One address, not a list. The gateway emits one delivery event per
    // recipient so that a multi-recipient message produces one row each.
    rcpt_list: emailSchema,
    rcpt_accepted: emailSchema,
    tls_forced: z.boolean(),
    tls: z.boolean(),
    auth: z.boolean(),
    host: hostSchema,
    ip: ipSchema,
    port: portSchema,
    response: z.string(),
    delay: z.number(),
});

export type Delivery = z.infer<typeof schemaDelivery>;
