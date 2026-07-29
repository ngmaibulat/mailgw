import { eq, lte } from "drizzle-orm";

import { db, sessions as sessionsTable } from "../db/index.ts";

// Session store, keyed by the session UUID carried in the signed cookie (see
// src/auth/login.ts and src/middleware/checkSession.ts).
//
// This used to be a plain in-process object, which meant every restart or deploy
// logged all operators out and auth broke outright with more than one replica.
// A fleet console is exactly the thing you want to run more than one of, so the
// store lives in the `Sessions` table (logservice migration 021).
export interface Session {
    email: string;
    expiresAt: number; // epoch ms; entry is dead once Date.now() >= this
}

// Server-side session lifetime. Kept in sync with the login cookie's maxAge so
// the cookie and the store expire together.
export const SESSION_TTL_MS = 8 * 60 * 60 * 1000; // 8h

export interface SessionStore {
    create(id: string, session: Session): Promise<void>;
    get(id: string): Promise<Session | undefined>;
    delete(id: string): Promise<void>;
    sweep(): Promise<void>;
}

const dbSessionStore: SessionStore = {
    async create(id, session) {
        await db.insert(sessionsTable).values({
            id,
            email: session.email,
            expiresAt: new Date(session.expiresAt),
        });
    },

    // Expired rows are treated as absent and deleted on access, so a
    // re-presented stale cookie no longer authenticates even if the periodic
    // sweep hasn't run yet.
    async get(id) {
        const [row] = await db
            .select({
                email: sessionsTable.email,
                expiresAt: sessionsTable.expiresAt,
            })
            .from(sessionsTable)
            .where(eq(sessionsTable.id, id))
            .limit(1);

        if (!row) {
            return undefined;
        }
        const expiresAt = row.expiresAt.getTime();
        if (expiresAt <= Date.now()) {
            await db.delete(sessionsTable).where(eq(sessionsTable.id, id));
            return undefined;
        }
        return { email: row.email, expiresAt };
    },

    async delete(id) {
        await db.delete(sessionsTable).where(eq(sessionsTable.id, id));
    },

    // Drop expired rows so the table doesn't grow without bound when users
    // abandon sessions without logging out. Called on an interval from app.ts.
    async sweep() {
        await db
            .delete(sessionsTable)
            .where(lte(sessionsTable.expiresAt, new Date()));
    },
};

// Swappable so `app.inject()` tests can run the auth gate without a database.
// Returns the previous store, so a test can restore it.
let store: SessionStore = dbSessionStore;

export function setSessionStore(next: SessionStore): SessionStore {
    const previous = store;
    store = next;
    return previous;
}

export async function createSession(
    sessionID: string,
    session: Session,
): Promise<void> {
    return store.create(sessionID, session);
}

export async function getSession(
    sessionID: string,
): Promise<Session | undefined> {
    return store.get(sessionID);
}

// Drop a session immediately (logout).
export async function deleteSession(sessionID: string): Promise<void> {
    return store.delete(sessionID);
}

export async function sweepSessions(): Promise<void> {
    return store.sweep();
}
