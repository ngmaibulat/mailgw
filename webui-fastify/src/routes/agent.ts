import type { FastifyInstance, FastifyRequest } from "fastify";
import { eq } from "drizzle-orm";

import { db, gateways, configVersions } from "../../db/index.ts";
import { uuidv4 } from "../adapter.ts";
import {
    authenticateGateway,
    canonicalString,
    fingerprintFromBase64,
    rawBodyOf,
    checkTimestamp,
    verifySignature,
} from "../agent/verify.ts";
import { RegisterInfo, ReportInfo } from "../validation/agent.ts";
import { zodErr } from "../validation/config.ts";

// The machine-facing API used by mailgw-go gateways.
//
// Registered at the ROOT scope in src/app.ts — deliberately outside both the
// cookie-session gate (`checkSession` would bounce a machine to /login) and the
// audit-log hook (a fleet of gateways polling would flood the Logs table for no
// audit value, the same reason `isNoise()` already skips GET /api/*).
//
// Auth is an Ed25519 signature over a canonical string; see src/agent/verify.ts.

export default async function agentRoutes(fastify: FastifyInstance) {
    // Capture the raw body so the signature covers exactly the bytes sent.
    // Scope-local: the global JSON parser is untouched.
    fastify.addContentTypeParser(
        "application/json",
        { parseAs: "string" },
        (request, body, done) => {
            (request as FastifyRequest & { rawBody?: string }).rawBody =
                body as string;
            try {
                done(null, body ? JSON.parse(body as string) : {});
            } catch (err) {
                done(err as Error, undefined);
            }
        },
    );

    // --- Registration -----------------------------------------------------
    //
    // Open by design: anything that can reach us may ask to join. What it gets
    // is a `pending` row — no configuration, no credentials — until an operator
    // approves its fingerprint.
    //
    // The request must still be signed by the key it presents. That proves the
    // caller holds the matching private key, so nobody can register someone
    // else's public key and squat their fingerprint.
    fastify.post("/register", async (request, reply) => {
        const parsed = RegisterInfo.safeParse(request.body);
        if (!parsed.success) {
            return reply
                .code(400)
                .send({ status: "error", message: zodErr(parsed.error) });
        }
        const info = parsed.data;

        const timestamp = request.headers["x-gw-timestamp"];
        const signature = request.headers["x-gw-signature"];
        const clock = checkTimestamp(
            typeof timestamp === "string" ? timestamp : undefined,
        );
        if (!clock.ok) {
            return reply
                .code(401)
                .send({ status: "error", message: clock.reason });
        }
        if (typeof signature !== "string" || !signature) {
            return reply
                .code(401)
                .send({ status: "error", message: "missing X-GW-Signature" });
        }

        const canonical = canonicalString(
            request.method,
            request.url,
            String(timestamp),
            rawBodyOf(request),
        );
        if (!verifySignature(info.pubkey, signature, canonical)) {
            return reply.code(401).send({
                status: "error",
                message: "signature does not match the presented pubkey",
            });
        }

        const fingerprint = fingerprintFromBase64(info.pubkey);
        const now = new Date();
        const systeminfo = {
            hostname: info.hostname ?? null,
            os: info.os ?? null,
            arch: info.arch ?? null,
            cpus: info.cpus ?? null,
            mem_bytes: info.mem_bytes ?? null,
            ip_addrs: info.ip_addrs ? info.ip_addrs.join(",") : null,
            version: info.version ?? null,
        };

        const [existing] = await db
            .select()
            .from(gateways)
            .where(eq(gateways.fingerprint, fingerprint))
            .limit(1);

        // Re-registration is idempotent and refreshes systeminfo — a gateway
        // restarting or moving host must not re-enter the approval queue, and
        // must never be able to reset its own status.
        if (existing) {
            await db
                .update(gateways)
                .set({ ...systeminfo, last_seen: now })
                .where(eq(gateways.id, existing.id));

            return reply.send({
                status: "ok",
                gateway_uid: existing.gateway_uid,
                fingerprint,
                approval: existing.status,
            });
        }

        const gateway_uid = uuidv4();
        await db.insert(gateways).values({
            gateway_uid,
            // A human-friendly default; the operator can rename it on approval.
            name: info.hostname ?? gateway_uid,
            fingerprint,
            pubkey: info.pubkey,
            status: "pending",
            ...systeminfo,
            first_seen: now,
            last_seen: now,
        });

        request.log.info(
            { gateway_uid, fingerprint, hostname: info.hostname },
            "gateway registered, awaiting approval",
        );

        return reply.code(201).send({
            status: "ok",
            gateway_uid,
            fingerprint,
            approval: "pending",
        });
    });

    // --- Signed routes ----------------------------------------------------
    // Encapsulated so the signature preHandler cannot leak onto /register.
    await fastify.register(async (signed) => {
        signed.addHook("preHandler", authenticateGateway);

        // The cheap poll. Answers a pending gateway too — that is how it learns
        // it is still waiting, and it is what the local status page displays.
        signed.get("/status", async (request, reply) => {
            const gateway = request.gateway;
            if (!gateway) {
                return reply
                    .code(401)
                    .send({ status: "error", message: "unauthenticated" });
            }

            await db
                .update(gateways)
                .set({ last_seen: new Date() })
                .where(eq(gateways.id, gateway.id));

            let desired: { version: number; sha256: string } | null = null;
            if (gateway.desired_version_id) {
                const [row] = await db
                    .select({
                        version: configVersions.version,
                        sha256: configVersions.bundle_sha256,
                    })
                    .from(configVersions)
                    .where(eq(configVersions.id, gateway.desired_version_id))
                    .limit(1);
                desired = row ?? null;
            }

            return reply.send({
                status: "ok",
                approval: gateway.status,
                desired_version_id: gateway.desired_version_id,
                desired_version: desired?.version ?? null,
                bundle_sha256: desired?.sha256 ?? null,
                applied_version_id: gateway.applied_version_id,
            });
        });

        // The configuration itself. This is the one route approval gates.
        signed.get("/config", async (request, reply) => {
            const gateway = request.gateway;
            if (!gateway) {
                return reply
                    .code(401)
                    .send({ status: "error", message: "unauthenticated" });
            }
            if (gateway.status !== "approved") {
                return reply.code(403).send({
                    status: "error",
                    message: `gateway is ${gateway.status}`,
                    approval: gateway.status,
                });
            }
            if (!gateway.desired_version_id) {
                return reply.code(404).send({
                    status: "error",
                    message: "no configuration has been deployed yet",
                });
            }

            const [row] = await db
                .select()
                .from(configVersions)
                .where(eq(configVersions.id, gateway.desired_version_id))
                .limit(1);

            if (!row) {
                return reply.code(404).send({
                    status: "error",
                    message: "the deployed configuration version is missing",
                });
            }

            return reply.send({
                status: "ok",
                version_id: row.id,
                version: row.version,
                bundle_sha256: row.bundle_sha256,
                // Stored as text and returned parsed, so the gateway sees the
                // same object the composer built.
                bundle: JSON.parse(row.bundle),
            });
        });

        // Heartbeat: what the gateway is actually running, and why not, if not.
        signed.post("/report", async (request, reply) => {
            const gateway = request.gateway;
            if (!gateway) {
                return reply
                    .code(401)
                    .send({ status: "error", message: "unauthenticated" });
            }

            const parsed = ReportInfo.safeParse(request.body);
            if (!parsed.success) {
                return reply
                    .code(400)
                    .send({ status: "error", message: zodErr(parsed.error) });
            }
            const report = parsed.data;

            // Metrics are accepted and logged now; persisting the time series
            // is M6. Reporting them early keeps the gateway's client stable.
            const changes: Record<string, unknown> = { last_seen: new Date() };
            if (report.applied_version_id !== undefined) {
                changes.applied_version_id = report.applied_version_id;
            }
            if (report.apply_error !== undefined) {
                changes.apply_error = report.apply_error;
            }
            if (report.restart_required !== undefined) {
                changes.restart_required = report.restart_required;
            }
            if (report.version !== undefined) {
                changes.version = report.version;
            }

            await db
                .update(gateways)
                .set(changes)
                .where(eq(gateways.id, gateway.id));

            return reply.send({ status: "ok" });
        });
    });
}
