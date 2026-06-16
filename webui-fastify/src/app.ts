import path from "node:path";
import { fileURLToPath } from "node:url";
import type { SecureServerOptions } from "node:http2";

import Fastify from "fastify";
import type {
    FastifyError,
    FastifyInstance,
    FastifyServerOptions,
} from "fastify";
import fastifyStatic from "@fastify/static";
import fastifyFormbody from "@fastify/formbody";
import fastifyCookie from "@fastify/cookie";
import fastifyView from "@fastify/view";
import fastifyHelmet from "@fastify/helmet";
import fastifyRateLimit from "@fastify/rate-limit";
import * as pug from "pug";

import { checkSession } from "./middleware/checkSession.ts";
import { logger } from "./middleware/logger.ts";
import { sweepSessions } from "./globals.ts";

import { login } from "./auth/login.ts";
import { logout } from "./auth/logout.ts";
import { profileForm, profileSubmit } from "./auth/profile.ts";
import { setupForm, setupSubmit } from "./auth/setup.ts";
import { formLogin } from "./forms/formLogin.ts";

import rootRoutes from "./routes/root.ts";
import logRoutes from "./routes/log.ts";
import apiRoutes from "./routes/api.ts";
import relayConfigRoutes from "./routes/config-relay.ts";
import userRoutes from "./routes/users.ts";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const projectRoot = path.join(__dirname, "..");
const templateDir =
    process.env.TEMPLATE_DIR || path.join(projectRoot, "templates", "pug");
const publicDir = path.join(projectRoot, "public");

// `Fastify()` is overloaded per transport; `build()` is also called by index.ts
// with `{ http2, https }` to serve native HTTP/2 over TLS. Widen the base
// server options with the http2/https keys so callers can pass them; the
// factory still inspects them at runtime to pick the right server.
export type BuildOptions = FastifyServerOptions & {
    http2?: boolean;
    https?: SecureServerOptions;
};

export async function build(opts: BuildOptions = {}): Promise<FastifyInstance> {
    // `TRUSTED_PROXIES` = comma-separated IPs/CIDRs of the reverse proxies in
    // front of the webui. When set, enable Fastify's `trustProxy` with that list
    // so `request.ip`/`request.protocol` come from `X-Forwarded-*` (correct
    // client IP in the audit log + the `secure` cookie works behind TLS
    // termination). Left unset (the default), the app reads the raw socket — the
    // right choice when it terminates TLS itself, and avoids trusting spoofable
    // `X-Forwarded-For` from arbitrary clients. We pass the explicit list (never
    // `true`) so only the named proxies are trusted.
    const trustedProxies = (process.env.TRUSTED_PROXIES ?? "")
        .split(",")
        .map((entry) => entry.trim())
        .filter(Boolean);

    const app = Fastify({
        ...(opts as FastifyServerOptions),
        ...(trustedProxies.length > 0 ? { trustProxy: trustedProxies } : {}),
    });

    // Required — `src/checkenv.ts` already refuses to boot the server without it.
    // Fail loudly here too (instead of the old `|| "sign"` fallback) so any path
    // that builds the app without checkenv, e.g. tests, can't run on a weak
    // hardcoded secret.
    const cookieSecret = process.env.SIGN_COOKIE;
    if (!cookieSecret) {
        throw new Error("SIGN_COOKIE is required (cookie signing secret)");
    }

    // Security headers on every response (registered at the root so it covers
    // static assets, /health, and all routes). Helmet's defaults are enforced —
    // HSTS, X-Content-Type-Options, X-Frame-Options, Referrer-Policy,
    // Cross-Origin-{Opener,Resource}-Policy, etc.
    //
    // CSP is shipped in REPORT-ONLY mode: the policy is tailored to this UI
    // (all scripts are same-origin /lib + /js, inline style attributes are used,
    // w2ui uses data: image URIs) but reportOnly means a missed directive logs a
    // browser-console violation instead of breaking the grids. Verify there are
    // no violations in the browser, then flip `reportOnly` to false to enforce.
    // The main unknown is whether w2ui/jQuery needs `'unsafe-eval'` in scriptSrc.
    await app.register(fastifyHelmet, {
        contentSecurityPolicy: {
            reportOnly: true,
            directives: {
                defaultSrc: ["'self'"],
                scriptSrc: ["'self'"],
                styleSrc: ["'self'", "'unsafe-inline'"],
                imgSrc: ["'self'", "data:"],
                fontSrc: ["'self'"],
                objectSrc: ["'none'"],
                frameAncestors: ["'none'"],
            },
        },
    });

    // Parsers/decorators registered at the root are inherited by every child
    // encapsulation context below. (JSON parsing is built into Fastify.)
    await app.register(fastifyFormbody); // application/x-www-form-urlencoded
    await app.register(fastifyCookie, { secret: cookieSecret }); // signed-cookie support
    await app.register(fastifyView, {
        engine: { pug },
        root: templateDir,
        viewExt: "pug",
    });

    // Brute-force protection. Registered with `global: false` so it gates only
    // the routes that opt in via `config.rateLimit` (currently `POST /login`) —
    // not every request. Keyed by `request.ip` (correct behind `trustProxy`).
    // When the limit is hit the plugin *throws* a 429 error, which lands in our
    // `setErrorHandler` below: form posts are redirected back to the login page
    // with `?msg=` (styled alert), JSON/API clients get the 429 status.
    await app.register(fastifyRateLimit, { global: false });

    // Render handler exceptions as a friendly page instead of falling through to
    // Fastify's default bare-JSON 500. API/JSON clients still get JSON; 5xx
    // messages are kept generic so internals aren't leaked, while 4xx (e.g. a
    // validation error) shows its own message.
    app.setErrorHandler((err: FastifyError, request, reply) => {
        const statusCode =
            err.statusCode && err.statusCode >= 400 ? err.statusCode : 500;
        request.log.error(err);

        const accept = String(request.headers.accept ?? "");
        const wantsJson =
            accept.includes("application/json") ||
            request.url.startsWith("/api/");

        // Rate-limit (@fastify/rate-limit) rejections surface here as 429s.
        // Send browser form posts back to the login form so they see the styled
        // alert instead of a bare error page; programmatic clients keep the 429.
        if (statusCode === 429 && !wantsJson) {
            return reply.redirect("/login?msg=TooManyAttempts");
        }

        if (wantsJson) {
            return reply
                .code(statusCode)
                .send({ status: "error", message: err.message });
        }

        const message =
            statusCode >= 500
                ? "An unexpected error occurred. Please try again."
                : err.message;
        return reply.code(statusCode).view("util/error", {
            statusCode,
            message,
        });
    });

    // Sweep expired sessions periodically so the in-memory store can't grow
    // without bound. Unref'd so it never keeps the process (or a test) alive;
    // cleared on close.
    const sweepTimer = setInterval(sweepSessions, 15 * 60 * 1000);
    sweepTimer.unref?.();
    app.addHook("onClose", async () => clearInterval(sweepTimer));

    // Static assets are public and intentionally NOT request-logged (matches the
    // Express version, where express.static ran before the logger middleware).
    // `index: false` keeps the plugin from claiming GET "/" so the dashboard
    // route below can own it.
    await app.register(fastifyStatic, {
        root: publicDir,
        prefix: "/",
        index: false,
    });

    // Unauthenticated liveness probe for Docker/k8s. Registered at the root
    // scope so it sits outside both the auth gate and the audit-log hook — it
    // must stay reachable without a session and shouldn't flood the Logs table.
    // Deliberately does NOT touch the DB (liveness, not readiness): a DB blip
    // shouldn't cause the orchestrator to kill an otherwise-healthy process.
    app.get("/health", async () => ({ status: "ok" }));

    // Everything except static assets is request-logged. Adding the hook inside
    // this child scope keeps it off the static routes registered above.
    await app.register(async (logged) => {
        logged.addHook("onRequest", logger);

        // Public (unauthenticated) routes.
        logged.get("/login", formLogin);
        // Rate-limited to blunt password brute-forcing (per-IP). The global
        // plugin above is `global: false`, so only this opted-in route is gated.
        logged.post("/login", {
            config: {
                rateLimit: {
                    max: Number(process.env.LOGIN_RATE_MAX ?? 5),
                    timeWindow: process.env.LOGIN_RATE_WINDOW ?? "1 minute",
                },
            },
            handler: login,
        });
        logged.get("/logout", logout);

        // First-run bootstrap — unauthenticated, but each handler refuses once
        // any user exists, so it's not an open registration endpoint.
        logged.get("/setup", setupForm);
        logged.post("/setup", setupSubmit);

        // Protected routes — encapsulated scope guarded by the session check.
        // The preHandler hook here does not leak to the public routes above.
        await logged.register(async (secured) => {
            secured.addHook("preHandler", checkSession);

            // Authenticated account page — shows the current user and a
            // change-password form. Gated here (not in the public block above)
            // because it must know who is logged in.
            secured.get("/profile", profileForm);
            secured.post("/profile", profileSubmit);

            await secured.register(rootRoutes);
            await secured.register(logRoutes, { prefix: "/log" });
            await secured.register(apiRoutes, { prefix: "/api" });
            await secured.register(relayConfigRoutes, { prefix: "/config" });
            await secured.register(userRoutes, { prefix: "/users" });
        });
    });

    return app;
}
