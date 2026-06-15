import "./errhandler.mjs";
import spdy from "spdy";
import fs from "node:fs";
import * as dotenv from "dotenv";

import { app } from "./app.mjs";

dotenv.config();
const port = process.env.PORT || 3000;

const options = {
    key: fs.readFileSync("./certs/server.key"),
    cert: fs.readFileSync("./certs/server.crt"),

    // **optional** SPDY-specific options
    spdy: {
        protocols: ["h2", "spdy/3.1", "http/1.1"],
        plain: false,

        // **optional**
        // Parse first incoming X_FORWARDED_FOR frame and put it to the
        // headers of every request.
        // NOTE: Use with care! This should not be used without some proxy that
        // will *always* send X_FORWARDED_FOR
        "x-forwarded-for": true,

        connection: {
            windowSize: 1024 * 1024, // Server's window size

            // **optional** if true - server will send 3.1 frames on 3.0 *plain* spdy
            autoSpdy31: false,
        },
    },
};

//@ts-ignore
const server = spdy.createServer(options, app);

server.listen(port);
