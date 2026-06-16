import "./src/checkenv.ts"; // loads + validates env; MUST be first so it runs before db/index.ts is evaluated (ESM imports are hoisted)

import { closeDb } from "./db/index.ts";
import { createUser } from "./src/auth/users.ts";

if (process.argv.length < 4) {
    console.error("Usage node create_user.ts <username> <password>");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];

try {
    await createUser(email, pass);
    console.log("User created!");
} catch (err) {
    console.error("Error creating user:", (err as Error).message);
}

await closeDb();
