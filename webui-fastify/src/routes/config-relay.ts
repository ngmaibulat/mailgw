import type { FastifyInstance } from "fastify";

import { CtrlRelayGroup } from "../controllers/CtrlRelayGroup.ts";
import { CtrlRelay } from "../controllers/CtrlRelay.ts";
import { CtrlProfile } from "../controllers/CtrlProfile.ts";
import {
    CtrlCredential,
    CtrlCredentialSet,
} from "../controllers/CtrlCredential.ts";
import { requireAdmin } from "../auth/roles.ts";

export default async function relayConfigRoutes(fastify: FastifyInstance) {
    const ctrlRelayGroup = new CtrlRelayGroup();
    const ctrlRelay = new CtrlRelay();
    const ctrlProfile = new CtrlProfile();
    const ctrlCredentialSet = new CtrlCredentialSet();
    const ctrlCredential = new CtrlCredential();

    const adminOnly = { preHandler: requireAdmin };

    ///////////////////////////////////////////////////////

    fastify.get("/relay/create/:group_id", ctrlRelay.create);
    fastify.post("/relay/create/:group_id", ctrlRelay.createHandle);

    fastify.get("/relay/edit/:id", ctrlRelay.edit);
    fastify.post("/relay/edit/:id", ctrlRelay.editHandle);

    fastify.get("/relay/delete/:id", ctrlRelay.delete);
    fastify.post("/relay/delete/:id", ctrlRelay.deleteHandle);

    ///////////////////////////////////////////////////////

    fastify.get("/relaygrp/create", ctrlRelayGroup.create);
    fastify.post("/relaygrp/create", ctrlRelayGroup.createHandle);

    fastify.get("/relaygrp/edit/:id", ctrlRelayGroup.edit);
    fastify.post("/relaygrp/edit/:id", ctrlRelayGroup.editHandle);

    fastify.get("/relaygrp/delete/:id", ctrlRelayGroup.delete);
    fastify.post("/relaygrp/delete/:id", ctrlRelayGroup.deleteHandle);

    fastify.get("/relaygrp/:id", ctrlRelayGroup.details);
    fastify.get("/relaygrp", ctrlRelayGroup.index);

    ///////////////////////////////////////////////////////

    // Reusable configuration blocks assigned to gateways. This replaces the old
    // `/config/routing` notimpl stub: routing is now a `ruleset` profile
    // carrying mailgw-go's routing.yaml, not a four-field Haraka table.
    fastify.get("/profiles/create", adminOnly, ctrlProfile.create);
    fastify.post("/profiles/create", adminOnly, ctrlProfile.createHandle);

    fastify.get("/profiles/edit/:id", adminOnly, ctrlProfile.edit);
    fastify.post("/profiles/edit/:id", adminOnly, ctrlProfile.editHandle);

    fastify.get("/profiles/delete/:id", adminOnly, ctrlProfile.delete);
    fastify.post("/profiles/delete/:id", adminOnly, ctrlProfile.deleteHandle);

    fastify.get("/profiles", ctrlProfile.index);

    ///////////////////////////////////////////////////////

    // Inbound SMTP AUTH credentials. EVERY route here is admin-only, the index
    // included — unlike /profiles, whose listing is open to any signed-in user.
    // A list of submission usernames is a list of what to guess passwords
    // against, and a viewer has no reason to see one.
    fastify.get("/credentials/create", adminOnly, ctrlCredentialSet.create);
    fastify.post(
        "/credentials/create",
        adminOnly,
        ctrlCredentialSet.createHandle,
    );

    fastify.get("/credentials/edit/:id", adminOnly, ctrlCredentialSet.edit);
    fastify.post(
        "/credentials/edit/:id",
        adminOnly,
        ctrlCredentialSet.editHandle,
    );

    fastify.get("/credentials/delete/:id", adminOnly, ctrlCredentialSet.delete);
    fastify.post(
        "/credentials/delete/:id",
        adminOnly,
        ctrlCredentialSet.deleteHandle,
    );

    fastify.get("/credential/create/:set_id", adminOnly, ctrlCredential.create);
    fastify.post(
        "/credential/create/:set_id",
        adminOnly,
        ctrlCredential.createHandle,
    );

    fastify.get("/credential/edit/:id", adminOnly, ctrlCredential.edit);
    fastify.post("/credential/edit/:id", adminOnly, ctrlCredential.editHandle);

    fastify.get("/credential/delete/:id", adminOnly, ctrlCredential.delete);
    fastify.post(
        "/credential/delete/:id",
        adminOnly,
        ctrlCredential.deleteHandle,
    );

    // After the static paths above, or ":id" would swallow "create".
    fastify.get("/credentials/:id", adminOnly, ctrlCredentialSet.details);
    fastify.get("/credentials", adminOnly, ctrlCredentialSet.index);

    // Kept so existing bookmarks and the old nav entry land somewhere useful.
    fastify.get("/routing", async (_request, reply) => {
        return reply.redirect("/config/profiles");
    });
}
