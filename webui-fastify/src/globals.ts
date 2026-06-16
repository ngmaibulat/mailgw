// In-memory session store. Keyed by the session UUID set in the signed cookie;
// the value carries the logged-in user's email and an absolute expiry (see
// src/auth/login.ts and src/middleware/checkSession.ts).
export interface Session {
    email: string;
    expiresAt: number; // epoch ms; entry is dead once Date.now() >= this
}

export const sessions: Record<string, Session> = {};

// Server-side session lifetime. Kept in sync with the login cookie's maxAge so
// the cookie and the store expire together.
export const SESSION_TTL_MS = 8 * 60 * 60 * 1000; // 8h

// Look up a live session. Expired entries are treated as absent and pruned on
// access, so a re-presented stale cookie no longer authenticates.
export function getSession(sessionID: string): Session | undefined {
    const session = sessions[sessionID];
    if (!session) {
        return undefined;
    }
    if (session.expiresAt <= Date.now()) {
        delete sessions[sessionID];
        return undefined;
    }
    return session;
}

// Drop a session immediately (logout).
export function deleteSession(sessionID: string): void {
    delete sessions[sessionID];
}

// Sweep expired sessions so the store doesn't grow without bound when users
// abandon sessions without logging out. Called on an interval from app.ts.
export function sweepSessions(): void {
    const now = Date.now();
    for (const [id, session] of Object.entries(sessions)) {
        if (session.expiresAt <= now) {
            delete sessions[id];
        }
    }
}
