import { createInsertSchema } from "drizzle-zod";
// drizzle-zod 0.8 emits zod v4 schemas, so extend/refine with the v4 API
// (zod 3.25+ ships it at "zod/v4"). The app's other schemas use classic v3 —
// they're independent, so the two coexist fine.
import { z, type ZodError } from "zod/v4";

import { relays, relayGroups } from "../../db/index.ts";

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
