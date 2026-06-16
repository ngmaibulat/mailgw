import type { FastifyInstance, FastifyReply, FastifyRequest } from "fastify";

import * as logservice from "../logservice.ts";
import { parseSearchRequest } from "../validation/search.ts";

type SearchQuery = { request?: string };

// Read-only query endpoints backing the log-viewer grids (public/js/log-*.js).
// All are proxied to logservice, which owns the log data (event ingestion + the
// MD5 filter live there). The webui no longer reads any log table directly.
//
// The frontend's `?request=<json>` is validated against logservice's accepted
// search shape (src/validation/search.ts) before forwarding — a malformed
// payload gets a 400 here instead of being bounced off logservice.
//
// Note the path remaps: queue -> logservice /api/transaction (queue events are
// stored as Transaction rows), and hashlookups -> /api/hashlookup (HashLookups
// joined to its Transaction for sender/rcpt).
async function proxySearch(
    request: FastifyRequest,
    reply: FastifyReply,
    path: string,
) {
    const parsed = parseSearchRequest((request.query as SearchQuery).request);
    if (!parsed.ok) {
        return reply.code(400).send({ status: "error", message: parsed.error });
    }
    return reply.send(await logservice.search(path, parsed.value));
}

export default async function apiRoutes(fastify: FastifyInstance) {
    fastify.get("/connection", (request, reply) =>
        proxySearch(request, reply, "/api/connection"),
    );

    fastify.get("/delivery", (request, reply) =>
        proxySearch(request, reply, "/api/delivery"),
    );

    fastify.get("/queue", (request, reply) =>
        proxySearch(request, reply, "/api/transaction"),
    );

    fastify.get("/hashlookups", (request, reply) =>
        proxySearch(request, reply, "/api/hashlookup"),
    );
}
