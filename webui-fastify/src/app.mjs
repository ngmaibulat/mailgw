import path from "node:path";
import { fileURLToPath } from "node:url";

import Fastify from "fastify";
import fastifyStatic from "@fastify/static";
import fastifyFormbody from "@fastify/formbody";
import fastifyCookie from "@fastify/cookie";
import fastifyView from "@fastify/view";
import * as pug from "pug";

import { checkSession } from "./middleware/checkSession.mjs";
import { logger } from "./middleware/logger.mjs";

import { login } from "./auth/login.mjs";
import { logout } from "./auth/logout.mjs";
import { profile } from "./auth/profile.mjs";
import { formLogin } from "./forms/formLogin.mjs";

import rootRoutes from "./routes/root.mjs";
import logRoutes from "./routes/log.mjs";
import apiRoutes from "./routes/api.mjs";
import relayConfigRoutes from "./routes/config-relay.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const projectRoot = path.join(__dirname, "..");
const templateDir = process.env.TEMPLATE_DIR || path.join(projectRoot, "templates", "pug");
const publicDir = path.join(projectRoot, "public");

export async function build(opts = {}) {
    const app = Fastify(opts);

    const cookieSecret = process.env.SIGN_COOKIE || "sign";

    // Parsers/decorators registered at the root are inherited by every child
    // encapsulation context below. (JSON parsing is built into Fastify.)
    await app.register(fastifyFormbody); // application/x-www-form-urlencoded
    await app.register(fastifyCookie, { secret: cookieSecret }); // signed-cookie support
    await app.register(fastifyView, {
        engine: { pug },
        root: templateDir,
        viewExt: "pug",
    });

    // Static assets are public and intentionally NOT request-logged (matches the
    // Express version, where express.static ran before the logger middleware).
    // `index: false` keeps the plugin from claiming GET "/" so the dashboard
    // route below can own it.
    await app.register(fastifyStatic, {
        root: publicDir,
        prefix: "/",
        index: false,
    });

    // Everything except static assets is request-logged. Adding the hook inside
    // this child scope keeps it off the static routes registered above.
    await app.register(async (logged) => {
        logged.addHook("onRequest", logger);

        // Public (unauthenticated) routes.
        logged.get("/login", formLogin);
        logged.post("/login", login);
        logged.get("/logout", logout);
        logged.get("/profile", profile);

        // Protected routes — encapsulated scope guarded by the session check.
        // The preHandler hook here does not leak to the public routes above.
        await logged.register(async (secured) => {
            secured.addHook("preHandler", checkSession);

            await secured.register(rootRoutes);
            await secured.register(logRoutes, { prefix: "/log" });
            await secured.register(apiRoutes, { prefix: "/api" });
            await secured.register(relayConfigRoutes, { prefix: "/config" });
        });
    });

    return app;
}
