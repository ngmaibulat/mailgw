import db from "$/db";

try {
    const result = await db.unsafe("SELECT VERSION() AS version");

    console.log("DB connection successful ✅");
    console.log("MariaDB version:", result);
} catch (err) {
    console.error("DB connection failed ❌");
    console.error(err);
    process.exit(1);
}
