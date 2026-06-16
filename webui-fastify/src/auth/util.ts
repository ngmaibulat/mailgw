import { eq } from "drizzle-orm";

import { bcrypt } from "../adapter.ts";
import { db, users } from "../../db/index.ts";

export async function checkAuth(email: string, pass: string): Promise<boolean> {
    const [record] = await db
        .select()
        .from(users)
        .where(eq(users.email, email))
        .limit(1);

    if (!record || record.hash == null) {
        console.error(`User not found: ${email}`);
        return false;
    }

    return await bcrypt.compare(pass, record.hash);
}
