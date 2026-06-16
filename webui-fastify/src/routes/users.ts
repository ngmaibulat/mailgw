import type { FastifyInstance } from "fastify";

import { CtrlUser } from "../controllers/CtrlUser.ts";

// Admin user management (list / create / edit / delete). Mounted under /users
// inside the secured (checkSession-gated) scope in src/app.ts.
export default async function userRoutes(fastify: FastifyInstance) {
    const ctrl = new CtrlUser();

    fastify.get("/create", ctrl.create);
    fastify.post("/create", ctrl.createHandle);

    fastify.get("/edit/:id", ctrl.edit);
    fastify.post("/edit/:id", ctrl.editHandle);

    fastify.get("/delete/:id", ctrl.delete);
    fastify.post("/delete/:id", ctrl.deleteHandle);

    fastify.get("/", ctrl.index);
}
