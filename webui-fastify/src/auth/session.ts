import type { FastifyRequest } from "fastify";

import { getSession } from "../globals.ts";

// Resolve the logged-in user's email from the signed `session` cookie, or null
// when there's no valid/live session. Signed cookies aren't auto-decoded, so we
// unsign the raw cookie via @fastify/cookie's request.unsignCookie (same as the
// checkSession gate). Shared by `checkSession` (the auth boundary) and handlers
// that need to know *who* is logged in (e.g. /profile).
export function sessionEmail(request: FastifyRequest): string | null {
    const raw = request.cookies?.session;
    const unsigned = raw
        ? request.unsignCookie(raw)
        : { valid: false as const, value: null };
    const sessionID = unsigned.valid ? unsigned.value : null;
    if (!sessionID) {
        return null;
    }
    return getSession(sessionID)?.email ?? null;
}
