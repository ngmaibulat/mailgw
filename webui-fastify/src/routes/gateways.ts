import type { FastifyInstance } from "fastify";

import { CtrlGateway } from "../controllers/CtrlGateway.ts";
import { requireAdmin } from "../auth/roles.ts";

// Fleet inventory and configuration deployment.
//
// Registered inside the secured (cookie-session) scope, so every route here
// already requires a login. Reads are open to any logged-in user; everything
// that changes the fleet is additionally gated on the admin role — approving a
// gateway is what lets it pull relay credentials, so it must not be something
// a read-only account can do.
export default async function gatewayRoutes(fastify: FastifyInstance) {
    const ctrl = new CtrlGateway();

    const adminOnly = { preHandler: requireAdmin };

    fastify.get("/", ctrl.index);

    fastify.post("/:id/status", adminOnly, ctrl.setStatus);
    fastify.post("/:id/rename", adminOnly, ctrl.rename);
    fastify.post("/:id/assignments", adminOnly, ctrl.saveAssignments);
    fastify.post("/:id/deploy", adminOnly, ctrl.deploy);
    fastify.post("/:id/rollback", adminOnly, ctrl.rollback);

    fastify.get("/:id/delete", adminOnly, ctrl.delete);
    fastify.post("/:id/delete", adminOnly, ctrl.deleteHandle);

    // Registered last: a static segment above must win over ":id".
    fastify.get("/:id", ctrl.details);
}
