import "./src/checkenv.mjs"; // loads + validates env; MUST be first so it runs before db/index.mjs is evaluated (ESM imports are hoisted)

import bcrypt from "bcryptjs";
import { db, users, closeDb } from "./db/index.mjs";

if (process.argv.length < 4) {
    console.error("Usage node create_user.mjs <username> <password>");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];

const salt = await bcrypt.genSalt(10);
const hash = await bcrypt.hash(pass, salt);

try {
    await db.insert(users).values({ email, hash });
    console.log("User created!");
} catch (err) {
    console.error("Error creating user:", err.message);
}

await closeDb();
