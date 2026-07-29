import { createInsertSchema } from "drizzle-zod";
// drizzle-zod 0.8 emits zod v4 schemas, so extend/refine with the v4 API
// (zod 3.25+ ships it at "zod/v4"). The app's other schemas use classic v3 —
// they're independent, so the two coexist fine.
import { z, type ZodError } from "zod/v4";

import { relays, relayGroups, configProfiles } from "../../db/index.ts";

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
    })
    .extend({
        host: z.string().min(1, "host is required"),
    });

export const relayGroupInsert = createInsertSchema(relayGroups)
    .pick({
        name: true,
        description: true,
    })
    .extend({
        name: z.string().min(1, "name is required"),
    });

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

// Form bodies arrive as strings; coerce the numeric relay fields before parse.
export function parseRelayBody(body: Record<string, unknown>) {
    return relayInsert.safeParse({
        ...body,
        group_id: Number(body.group_id),
        port: Number(body.port),
        priority: Number(body.priority),
    });
}

export function zodErr(error: ZodError): string {
    return error.issues
        .map((i) => `${i.path.join(".") || "field"}: ${i.message}`)
        .join("; ");
}
