import "./src/checkenv.ts"; // loads + validates env; MUST be first so it runs before db/index.ts is evaluated (ESM imports are hoisted)

import { checkAuth } from "./src/auth/util.ts";
import { closeDb } from "./db/index.ts";

if (process.argv.length < 4) {
    console.error("Usage node check_user.ts <username> <password>");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];

const authResult = await checkAuth(email, pass);

if (!authResult) {
    console.error("Auth failed");
} else {
    console.log("Auth success");
}

await closeDb();
