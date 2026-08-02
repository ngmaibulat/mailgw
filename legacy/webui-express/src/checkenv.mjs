import * as dotenv from "dotenv";

// Load .env before anything else reads process.env. This module is imported
// first in src/index.mjs so the check runs before the DB connection (which is
// established at import time) can fail or hang on missing config.
dotenv.config({ quiet: true }); // quiet:true suppresses dotenv v17's stdout tip banner

// Vars the app cannot start without. The DB ones feed Sequelize, which either
// throws ("Dialect needs to be explicitly supplied") or hangs on connect when
// they are absent.
const REQUIRED = [
    { name: "DB_DRIVER", hint: 'database dialect, e.g. "mysql" or "mariadb"' },
    { name: "DB_HOST", hint: "database host, e.g. 127.0.0.1" },
    { name: "DB_NAME", hint: "database name" },
    { name: "DB_USER", hint: "database user" },
    { name: "DB_PASS", hint: "database password" },
];

const missing = REQUIRED.filter(({ name }) => !process.env[name]);

if (missing.length > 0) {
    console.error("\nwebui cannot start: missing required environment variables\n");
    for (const { name, hint } of missing) {
        console.error(`  - ${name}  (${hint})`);
    }
    console.error(
        "\nSet them in a .env file (cwd) or the environment, then start again.\n"
    );
    process.exit(1);
}
