import { z } from "zod";

const emailSchema = z.string().email();
const domainSchema = z.string().regex(/^[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/);
const ipSchema = z.string().regex(/^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$/);
const portSchema = z.string().regex(/^[0-9]+$/);

export const schemaDelivery = z.object({
    uuid: z.string(),
    dt: z.number(),
    sender: emailSchema,
    rcpt_domain: domainSchema,
    rcpt_list: emailSchema,
    rcpt_accepted: emailSchema,
    tls_forced: z.boolean(),
    tls: z.boolean(),
    auth: z.boolean(),
    host: domainSchema,
    ip: ipSchema,
    port: portSchema,
    response: z.string(),
    delay: z.number(),
});

export type Delivery = z.infer<typeof schemaDelivery>;
