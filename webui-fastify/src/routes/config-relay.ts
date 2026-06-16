import type { FastifyInstance } from "fastify";

import { CtrlRelayGroup } from "../controllers/CtrlRelayGroup.ts";
import { CtrlRelay } from "../controllers/CtrlRelay.ts";

export default async function relayConfigRoutes(fastify: FastifyInstance) {
    const ctrlRelayGroup = new CtrlRelayGroup();
    const ctrlRelay = new CtrlRelay();

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

    fastify.get("/routing", async (_request, reply) => {
        return reply.view("util/notimpl", {});
    });
}
