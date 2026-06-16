import * as dotenv from "dotenv";

// Load .env before anything else reads process.env. This module is imported
// first in src/index.ts so the check runs before the DB connection (which is
// established at import time) can fail or hang on missing config.
dotenv.config({ quiet: true }); // quiet:true suppresses dotenv v17's stdout tip banner

// Vars the app cannot start without — they configure the mysql2 connection
// pool (db/index.ts). Validated here so startup fails with a clear message
// instead of an opaque connection error.
const REQUIRED = [
    { name: "DB_HOST", hint: "database host, e.g. 127.0.0.1" },
    { name: "DB_NAME", hint: "database name" },
    { name: "DB_USER", hint: "database user" },
    { name: "DB_PASS", hint: "database password" },
    { name: "SIGN_COOKIE", hint: "cookie signing secret (random string)" },
];

const missing = REQUIRED.filter(({ name }) => !process.env[name]);

if (missing.length > 0) {
    console.error(
        "\nwebui cannot start: missing required environment variables\n",
    );
    for (const { name, hint } of missing) {
        console.error(`  - ${name}  (${hint})`);
    }
    console.error(
        "\nSet them in a .env file (cwd) or the environment, then start again.\n",
    );
    process.exit(1);
}
