// In-memory session store. Keyed by the session UUID set in the signed cookie;
// the value carries the logged-in user's email (see src/auth/login.ts and
// src/middleware/checkSession.ts).
export interface Session {
    email: string;
}

export const sessions: Record<string, Session> = {};
