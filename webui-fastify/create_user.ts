import "./src/checkenv.ts"; // loads + validates env; MUST be first so it runs before db/index.ts is evaluated (ESM imports are hoisted)

import { closeDb } from "./db/index.ts";
import { createUser } from "./src/auth/users.ts";

if (process.argv.length < 4) {
    console.error("Usage node create_user.ts <username> <password> [role]");
    console.error("  role: admin (default) | viewer");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];
// This CLI is the break-glass path — it exists to get an operator back into a
// console they're locked out of, so it defaults to admin rather than viewer.
const role = process.argv[4] ?? "admin";

if (role !== "admin" && role !== "viewer") {
    console.error(`Unknown role ${JSON.stringify(role)}: use admin or viewer`);
    process.exit(1);
}

try {
    await createUser(email, pass, role);
    console.log(`User created (${role})!`);
} catch (err) {
    console.error("Error creating user:", (err as Error).message);
}

await closeDb();
