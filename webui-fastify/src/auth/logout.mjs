export async function logout(request, reply) {
    reply.clearCookie("session", { path: "/" });
    return reply.redirect("/");
}
