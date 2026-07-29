import type { FastifyInstance } from "fastify";

import { CtrlRelayGroup } from "../controllers/CtrlRelayGroup.ts";
import { CtrlRelay } from "../controllers/CtrlRelay.ts";
import { CtrlProfile } from "../controllers/CtrlProfile.ts";
import { requireAdmin } from "../auth/roles.ts";

export default async function relayConfigRoutes(fastify: FastifyInstance) {
    const ctrlRelayGroup = new CtrlRelayGroup();
    const ctrlRelay = new CtrlRelay();
    const ctrlProfile = new CtrlProfile();

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

    // Kept so existing bookmarks and the old nav entry land somewhere useful.
    fastify.get("/routing", async (_request, reply) => {
        return reply.redirect("/config/profiles");
    });
}
