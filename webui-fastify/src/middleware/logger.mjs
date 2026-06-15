import { db, logs } from "../../db/index.mjs";

// Fastify `onRequest` hook — writes one row per request to the Logs table.
// Only columns that exist in the `logs` schema are included (Sequelize silently
// dropped unknown attributes; Drizzle would reject them).
export function logger(request, reply, done) {
    const h = request.headers;
    const row = {
        url: `${request.protocol}://${h.host}${request.url}`,
        path: request.url.split("?")[0],
        query: request.url,
        method: request.method,
        src_ip: request.ip,
        src_port: request.socket.remotePort,
        referer: h.referrer || h.referer,
        userAgent: h["user-agent"],
        origin: h.origin,
        user: "-",
    };

    Promise.resolve(db.insert(logs).values(row)).catch((err) => console.error(err));

    if (process.env.NODE_ENV == "development") {
        console.log(row);
    }

    done();
}
