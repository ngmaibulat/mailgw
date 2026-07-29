import type { FastifyReply, FastifyRequest } from "fastify";

import { sessionEmail } from "./session.ts";
import { getUserRole } from "./users.ts";

export const ROLES = ["admin", "viewer"] as const;
export type Role = (typeof ROLES)[number];

// Fastify `preHandler` gating a route on the admin role.
//
// Registered *after* `checkSession` in the same scope, so by the time this runs
// a session already exists; the null branches are the narrow window where the
// session expired or the user was deleted between the two hooks.
//
// Deliberately fails closed: an unknown or missing role is not an admin. Before
// migration 020 every logged-in user could approve gateways, deploy config and
// read relay credentials — this is the split that ends that.
export async function requireAdmin(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    const email = await sessionEmail(request);
    if (!email) {
        return reply.redirect("/login");
    }

    const role = await getUserRole(email);
    if (role !== "admin") {
        const accept = String(request.headers.accept ?? "");
        const wantsJson =
            accept.includes("application/json") ||
            request.url.startsWith("/api/");
        if (wantsJson) {
            return reply.code(403).send({ error: "forbidden" });
        }
        return reply.code(403).view("util/error", {
            statusCode: 403,
            message: "This action requires an administrator account.",
        });
    }
}
