import { runMigrations } from "./src/migrate";

runMigrations().catch(err => {
    console.error("[migrate] failed:", err);
    process.exit(1);
});
