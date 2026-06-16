import type { FastifyReply, FastifyRequest } from "fastify";

import { sessionEmail } from "./session.ts";
import { checkAuth } from "./util.ts";
import { updatePassword } from "./users.ts";
import { ChangePassword } from "../validation/login.ts";
import { zodErr } from "../validation/config.ts";

// /profile is registered inside the secured (checkSession-gated) scope, so a
// session always exists here. `sessionEmail` can still theoretically return null
// if the cookie expired between the gate and the handler — treat that as logged
// out rather than rendering a profile for nobody.

export async function profileForm(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    const email = sessionEmail(request);
    if (!email) {
        return reply.redirect("/login");
    }
    return reply.view("forms/profile", { email });
}

export async function profileSubmit(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    const email = sessionEmail(request);
    if (!email) {
        return reply.redirect("/login");
    }

    const body = ChangePassword.safeParse(request.body);
    if (!body.success) {
        return reply.view("forms/profile", {
            email,
            error: zodErr(body.error),
        });
    }

    // Re-authenticate with the current password before changing it.
    if (!(await checkAuth(email, body.data.current))) {
        return reply.view("forms/profile", {
            email,
            error: "Current password is incorrect.",
        });
    }

    try {
        await updatePassword(email, body.data.pass);
    } catch (err) {
        request.log.error(err);
        return reply.view("forms/profile", {
            email,
            error: "Could not update password. Please try again.",
        });
    }

    return reply.view("forms/profile", {
        email,
        success: "Password updated.",
    });
}
