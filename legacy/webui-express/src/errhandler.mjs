import Exception from "../db/esmmodels/exception.mjs";

process.on("uncaughtException", async function (err) {
    console.error(new Date().toUTCString() + " Exception:", err.message);
    console.error(err.stack);

    const data = {
        product: "ngm-mailgw-webui",
        component: "webui",
        info: err.message,
    };

    const item = await Exception.create(data);
});
