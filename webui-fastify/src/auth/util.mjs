import { eq } from "drizzle-orm";

import { bcrypt } from "../adapter.js";
import { db, users } from "../../db/index.mjs";

export async function checkAuth(email, pass) {
    const [record] = await db
        .select()
        .from(users)
        .where(eq(users.email, email))
        .limit(1);

    if (!record) {
        console.error(`User not found: ${email}`);
        return false;
    }

    return bcrypt.compareSync(pass, record.hash);
}
