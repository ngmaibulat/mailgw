import "./checkenv.mjs"; // validates required env vars; must run before the DB-connecting imports below
import "./errhandler.mjs";
import https from "node:https";
import fs from "node:fs";

import { app } from "./app.mjs";

const port = process.env.PORT || 3000;

// HTTP/1.1 over TLS. Express is not compatible with Node's native http2 server
// (it patches the HTTP/1 IncomingMessage/ServerResponse prototypes), so this
// uses plain `https`. HTTP/2 lives in the Fastify rewrite (webui-fastify).
const options = {
    key: fs.readFileSync("./certs/server.key"),
    cert: fs.readFileSync("./certs/server.crt"),
};

const server = https.createServer(options, app);

server.listen(port, () => {
    console.log(`webui listening on https://localhost:${port}`);
});
