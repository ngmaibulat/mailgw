import { db, exceptions } from "../db/index.ts";

// Persist a top-level failure to the exceptions table. Best-effort: a DB error
// here must not throw out of the process-level handlers below.
async function persist(info: string): Promise<void> {
    try {
        await db.insert(exceptions).values({
            product: "ngm-mailgw-webui",
            component: "webui",
            info,
        });
    } catch (e) {
        console.error("failed to persist exception:", (e as Error).message);
    }
}

process.on("uncaughtException", async (err) => {
    console.error(`${new Date().toUTCString()} Exception:`, err.message);
    console.error(err.stack);
    await persist(err.message);
});

process.on("unhandledRejection", async (reason) => {
    const err = reason instanceof Error ? reason : new Error(String(reason));
    console.error(
        `${new Date().toUTCString()} Unhandled rejection:`,
        err.message,
    );
    console.error(err.stack);
    await persist(err.message);
});
