import { createInsertSchema } from "drizzle-zod";
// drizzle-zod 0.8 emits zod v4 schemas, so extend/refine with the v4 API
// (zod 3.25+ ships it at "zod/v4"). The app's other schemas use classic v3 —
// they're independent, so the two coexist fine.
import { z, type ZodError } from "zod/v4";

import {
    relays,
    relayGroups,
    configProfiles,
    credentialSets,
    smtpCredentials,
} from "../../db/index.ts";

// Outbound TLS policy for a relay leg. The names and the "unset means
// opportunistic" default match mailgw-go's relays.Relay.TLSPolicy(); a value it
// does not recognise is a hard config error there, so it is rejected here.
export const RELAY_TLS_POLICIES = [
    "",
    "none",
    "opportunistic",
    "required",
] as const;

// drizzle-zod derives these from the table definitions. `.pick()` whitelists the
// fields a form may set (no `id`/timestamps), and zod strips anything else — so
// `request.body` can no longer mass-assign columns. `.extend()` then makes the
// semantically-required fields non-empty (the DB columns are nullable, so
// drizzle-zod alone would accept ""), matching the forms' `required` attrs.
export const relayInsert = createInsertSchema(relays)
    .pick({
        group_id: true,
        host: true,
        port: true,
        priority: true,
        auth_user: true,
        auth_pass: true,
        tls: true,
        allow_insecure_auth: true,
        use_mx: true,
    })
    .extend({
        host: z.string().min(1, "host is required"),
        tls: z.enum(RELAY_TLS_POLICIES).optional(),
        allow_insecure_auth: z.boolean(),
        use_mx: z.boolean(),
    });

export const relayGroupInsert = createInsertSchema(relayGroups)
    .pick({
        name: true,
        description: true,
    })
    .extend({
        name: z.string().min(1, "name is required"),
    });

// bcrypt hashes at most 72 bytes of input and bcryptjs silently ignores the
// rest, while Go's golang.org/x/crypto/bcrypt — which is what the gateway
// verifies with — returns ErrPasswordTooLong. Refusing here keeps the console
// from accepting a password the fleet would reject, and from storing a hash
// that only matches the first 72 bytes of what the operator typed.
export const BCRYPT_MAX_PASSWORD_BYTES = 72;

export const credentialSetInsert = createInsertSchema(credentialSets)
    .pick({
        name: true,
        description: true,
    })
    .extend({
        name: z.string().min(1, "name is required"),
    });

// `hash` is not a form field: the form takes a password and the controller
// bcrypts it. Picking only `username` and `set_id` is what stops a crafted body
// from writing a hash of its own choosing.
export const credentialInsert = createInsertSchema(smtpCredentials)
    .pick({
        set_id: true,
        username: true,
    })
    .extend({
        username: z.string().min(1, "username is required"),
    });

// The password half, validated separately because it is optional on edit
// ("leave blank to keep") and required on create.
export const credentialPassword = z
    .string()
    .min(12, "password must be at least 12 characters")
    .refine(
        (p) => Buffer.byteLength(p, "utf8") <= BCRYPT_MAX_PASSWORD_BYTES,
        `password must be at most ${BCRYPT_MAX_PASSWORD_BYTES} bytes — bcrypt ignores anything beyond that`,
    );

// The three kinds of reusable config block a gateway can be assigned. `ruleset`
// and `server` are YAML text, `allowlist` is JSON — all stored verbatim and
// validated authoritatively by the gateway (see src/central/bundle.ts).
export const PROFILE_KINDS = ["ruleset", "allowlist", "server"] as const;
export type ProfileKind = (typeof PROFILE_KINDS)[number];

export const configProfileInsert = createInsertSchema(configProfiles)
    .pick({
        kind: true,
        name: true,
        description: true,
        body: true,
    })
    .extend({
        kind: z.enum(PROFILE_KINDS),
        name: z.string().min(1, "name is required").max(255),
        body: z.string().min(1, "body is required"),
    });

// Shape-only checks. The rule DSL is compiled by Go, so anything deeper would
// be a second source of truth that can disagree with the gateway — what we can
// usefully catch here is a body that is not even the right file format.
export function parseProfileBody(body: Record<string, unknown>) {
    const parsed = configProfileInsert.safeParse(body);
    if (!parsed.success) {
        return parsed;
    }
    if (parsed.data.kind === "allowlist") {
        try {
            const value = JSON.parse(parsed.data.body) as {
                allowed?: unknown;
            };
            if (!Array.isArray(value?.allowed)) {
                return {
                    success: false as const,
                    error: 'allowlist must be JSON of the form {"allowed": ["10.0.0.0/8", ...]}',
                };
            }
        } catch (err) {
            return {
                success: false as const,
                error: `allowlist is not valid JSON: ${(err as Error).message}`,
            };
        }
    }
    return parsed;
}

// Form bodies arrive as strings; coerce the numeric and checkbox relay fields
// before parse. An unchecked checkbox is absent from the body entirely, which is
// why this is a presence test rather than a value test.
export function parseRelayBody(body: Record<string, unknown>) {
    return relayInsert.safeParse({
        ...body,
        group_id: Number(body.group_id),
        port: Number(body.port),
        priority: Number(body.priority),
        allow_insecure_auth: Boolean(body.allow_insecure_auth),
        use_mx: Boolean(body.use_mx),
    });
}

export function parseCredentialBody(body: Record<string, unknown>) {
    return credentialInsert.safeParse({
        ...body,
        set_id: Number(body.set_id),
    });
}

export function zodErr(error: ZodError): string {
    return error.issues
        .map((i) => `${i.path.join(".") || "field"}: ${i.message}`)
        .join("; ");
}
