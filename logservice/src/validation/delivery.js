const z = require("zod");

const emailSchema = z.string().email();
const uuidSchema = z.string().uuid();
const domainSchema = z.string().regex(/^[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/);
const ipSchema = z.string().regex(/^(?:[0-9]{1,3}\.){3}[0-9]{1,3}$/);
const portSchema = z.string().regex(/^[0-9]+$/);
const booleanSchema = z.boolean();
const numberSchema = z.number();

const schemaDelivery = z.object({
    // uuid: uuidSchema,
    uuid: z.string(),
    dt: numberSchema,
    sender: emailSchema,
    rcpt_domain: domainSchema,
    rcpt_list: emailSchema,
    rcpt_accepted: emailSchema,
    tls_forced: booleanSchema,
    tls: booleanSchema,
    auth: booleanSchema,
    host: domainSchema,
    ip: ipSchema,
    port: portSchema,
    response: z.string(),
    delay: numberSchema,
});

module.exports = { schemaDelivery };
