import type { FastifyInstance, FastifyRequest } from "fastify";
import { eq } from "drizzle-orm";

import {
    db,
    gateways,
    configVersions,
    gatewayMetrics,
} from "../../db/index.ts";
import { uuidv4 } from "../adapter.ts";
import {
    authenticateGateway,
    canonicalString,
    fingerprintFromBase64,
    rawBodyOf,
    checkTimestamp,
    verifySignature,
} from "../agent/verify.ts";
import { onGatewayChange } from "../central/notify.ts";
import { RegisterInfo, ReportInfo } from "../validation/agent.ts";
import { zodErr } from "../validation/config.ts";

// How often a live socket re-reads its own gateway row.
//
// This is the cross-replica backstop, not the primary path: a deploy handled by
// this process reaches the socket instantly through the bus. Slow enough that a
// large fleet costs one trivial indexed query per gateway per interval.
const WS_RECHECK_MS = 10_000;

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

            // One transaction, so a gateway row can never carry a last_seen
            // newer than the snapshot that arrived with it — which is what a
            // stale-detection check on the fleet view reads.
            await db.transaction(async (tx) => {
                await tx
                    .update(gateways)
                    .set(changes)
                    .where(eq(gateways.id, gateway.id));

                // Absent rather than empty means the gateway has no registry
                // (an old build, or one started without one). Leave the
                // previous snapshot alone rather than overwriting it with
                // nothing — a missing sample is not the same as a reset fleet.
                if (report.metrics === undefined) return;

                const now = new Date();
                const body = JSON.stringify(report.metrics);
                await tx
                    .insert(gatewayMetrics)
                    .values({
                        gateway_id: gateway.id,
                        metrics: body,
                        updated_at: now,
                    })
                    // Keyed on gateway_id, so every later heartbeat replaces
                    // the row rather than accumulating history. See migration
                    // 024 for why this is a snapshot and not a time series.
                    .onDuplicateKeyUpdate({
                        set: { metrics: body, updated_at: now },
                    });
            });

            return reply.send({ status: "ok" });
        });

        // --- Change notification ------------------------------------------
        //
        // A gateway holds this open for the life of its process so a deploy
        // lands in milliseconds rather than on its next 15-second poll. The
        // upgrade request is signed exactly like every other route, so this
        // introduces no new authentication.
        //
        // Frames carry no state. The gateway is told "something changed" and
        // asks /agent/status what — which means a duplicated, delayed or
        // spurious notification costs one cheap request and can never deliver
        // wrong state. The gateway's poll loop is still running underneath, so
        // a socket that never connects changes nothing but latency.
        //
        // Approval is NOT required. A pending gateway learning the moment it is
        // approved is exactly the case this is most useful for.
        signed.get("/ws", { websocket: true }, (socket, request) => {
            const gateway = request.gateway;
            if (!gateway) {
                socket.close(1008, "unauthenticated");
                return;
            }

            // "" means the baseline has not been read yet, which is why the
            // first read is silent: the gateway polled /agent/status to get
            // here, so it already knows the current state.
            let lastSeen: string | null = null;

            const push = async () => {
                try {
                    const [row] = await db
                        .select({
                            status: gateways.status,
                            desired_version_id: gateways.desired_version_id,
                        })
                        .from(gateways)
                        .where(eq(gateways.id, gateway.id))
                        .limit(1);
                    if (!row) {
                        return;
                    }

                    // Only speak when something actually changed, so the slow
                    // timer below is silent on an idle fleet.
                    const state = `${row.status}:${row.desired_version_id ?? ""}`;
                    const first = lastSeen === null;
                    if (state === lastSeen) {
                        return;
                    }
                    lastSeen = state;
                    if (!first) {
                        socket.send(JSON.stringify({ event: "changed" }));
                    }
                } catch (err) {
                    request.log.warn(
                        { err, gateway: gateway.gateway_uid },
                        "gateway notification failed",
                    );
                }
            };

            const unsubscribe = onGatewayChange(gateway.id, () => {
                void push();
            });

            // The cross-replica safety net: a deploy handled by another console
            // replica never reaches this process's bus, so re-read the row
            // periodically. Slow on purpose — it is a backstop, and the
            // gateway's own poll is a second one behind it.
            const timer = setInterval(() => {
                void push();
            }, WS_RECHECK_MS);

            socket.on("close", () => {
                unsubscribe();
                clearInterval(timer);
            });
            socket.on("error", () => {
                unsubscribe();
                clearInterval(timer);
            });

            void push();
        });
    });
}
