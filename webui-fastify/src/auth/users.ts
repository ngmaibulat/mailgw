import { eq } from "drizzle-orm";

import { bcrypt } from "../adapter.ts";
import { db, users } from "../../db/index.ts";

// Number of users currently in the DB. Used by the first-run setup flow
// (src/auth/setup.ts) and the startup check (src/index.ts) to decide whether a
// first admin still needs creating.
export async function countUsers(): Promise<number> {
    return await db.$count(users);
}

// Create a user with a bcrypt-hashed password. Shared by the web setup route
// and the create_user.ts CLI so the hashing is identical in both.
export async function createUser(email: string, pass: string): Promise<void> {
    const salt = await bcrypt.genSalt(10);
    const hash = await bcrypt.hash(pass, salt);
    await db.insert(users).values({ email, hash });
}

// Replace a user's password with a fresh bcrypt hash. Used by /profile's
// change-password flow (the caller verifies the current password first).
export async function updatePassword(
    email: string,
    pass: string,
): Promise<void> {
    const salt = await bcrypt.genSalt(10);
    const hash = await bcrypt.hash(pass, salt);
    await db.update(users).set({ hash }).where(eq(users.email, email));
}

// --- Admin user management (src/controllers/CtrlUser.ts) -------------------
// These never select the password `hash` (it's not needed for listing/editing
// and shouldn't reach a template).

export async function listUsers() {
    return await db
        .select({
            id: users.id,
            email: users.email,
            createdAt: users.createdAt,
        })
        .from(users);
}

export async function getUser(id: number) {
    const [user] = await db
        .select({ id: users.id, email: users.email })
        .from(users)
        .where(eq(users.id, id))
        .limit(1);
    return user;
}

// Update a user by id. The email is always set; the password is only re-hashed
// when a new one is supplied ("leave blank to keep", like relay edit).
export async function updateUser(
    id: number,
    changes: { email: string; pass?: string },
): Promise<void> {
    const values: { email: string; hash?: string } = { email: changes.email };
    if (changes.pass) {
        const salt = await bcrypt.genSalt(10);
        values.hash = await bcrypt.hash(changes.pass, salt);
    }
    await db.update(users).set(values).where(eq(users.id, id));
}

export async function deleteUser(id: number): Promise<void> {
    await db.delete(users).where(eq(users.id, id));
}
