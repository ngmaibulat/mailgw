import { db, exceptions } from "../db/index.ts";

process.on("uncaughtException", async function (err) {
    console.error(new Date().toUTCString() + " Exception:", err.message);
    console.error(err.stack);

    try {
        await db.insert(exceptions).values({
            product: "ngm-mailgw-webui",
            component: "webui",
            info: err.message,
        });
    } catch (e) {
        console.error("failed to persist exception:", (e as Error).message);
    }
});
