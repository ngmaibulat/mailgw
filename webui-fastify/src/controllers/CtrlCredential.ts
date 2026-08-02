import type { FastifyReply, FastifyRequest } from "fastify";
import { asc, eq } from "drizzle-orm";

import { db, credentialSets, smtpCredentials } from "../../db/index.ts";
import { bcrypt } from "../adapter.ts";
import {
    credentialPassword,
    credentialSetInsert,
    parseCredentialBody,
    zodErr,
} from "../validation/config.ts";

type IdParams = { id: string };
type SetParams = { set_id: string };

// Cost 10, matching src/auth/users.ts. The gateway verifies with
// golang.org/x/crypto/bcrypt, which reads the cost out of the hash itself, so
// this is a console-side choice only — but one that lands on the gateway's SMTP
// path, where every AUTH command pays it.
const BCRYPT_COST = 10;

// Inbound SMTP AUTH credentials — the ones a submission client presents TO a
// gateway, as distinct from Relays.auth_pass, which is what a gateway presents
// to a relay.
//
// The difference decides everything about this controller: an inbound
// credential is only ever verified, so it is hashed here and never recoverable,
// and src/central/secrets.ts is deliberately not involved. A leaked bundle costs
// an offline bcrypt attack rather than a working login.
export class CtrlCredentialSet {
    async index(_request: FastifyRequest, reply: FastifyReply) {
        const data = await db
            .select()
            .from(credentialSets)
            .orderBy(asc(credentialSets.name));

        return reply.view("config/credential-index", { data });
    }

    async details(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [set] = await db
            .select()
            .from(credentialSets)
            .where(eq(credentialSets.id, id))
            .limit(1);
        if (!set) {
            return reply.redirect("/config/credentials");
        }

        const users = await db
            .select()
            .from(smtpCredentials)
            .where(eq(smtpCredentials.set_id, id))
            .orderBy(asc(smtpCredentials.username));

        return reply.view("config/credential-set", { set, users });
    }

    async create(_request: FastifyRequest, reply: FastifyReply) {
        return reply.view("config/credential-set-form", {
            action: "Create",
            data: { name: "", description: "" },
        });
    }

    async createHandle(request: FastifyRequest, reply: FastifyReply) {
        const parsed = credentialSetInsert.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("config/credential-set-form", {
                action: "Create",
                data: request.body,
                error: zodErr(parsed.error),
            });
        }

        await db.insert(credentialSets).values(parsed.data);
        return reply.redirect("/config/credentials");
    }

    async edit(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(credentialSets)
            .where(eq(credentialSets.id, id))
            .limit(1);
        if (!data) {
            return reply.redirect("/config/credentials");
        }

        return reply.view("config/credential-set-form", {
            action: "Update",
            data,
        });
    }

    async editHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const parsed = credentialSetInsert.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("config/credential-set-form", {
                action: "Update",
                data: { ...(request.body as object), id },
                error: zodErr(parsed.error),
            });
        }

        await db
            .update(credentialSets)
            .set(parsed.data)
            .where(eq(credentialSets.id, id));
        return reply.redirect("/config/credentials");
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(credentialSets)
            .where(eq(credentialSets.id, id))
            .limit(1);

        return reply.view("config/credential-set-delete", { data });
    }

    // Deleting a set takes its credentials with it. An assignment pointing at
    // it is deliberately left behind, matching CtrlProfile: composeBundle
    // resolves a missing set to "nothing assigned" and omits the key, rather
    // than failing the deploy.
    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        await db.delete(smtpCredentials).where(eq(smtpCredentials.set_id, id));
        await db.delete(credentialSets).where(eq(credentialSets.id, id));
        return reply.redirect("/config/credentials");
    }
}

export class CtrlCredential {
    async create(request: FastifyRequest, reply: FastifyReply) {
        const set_id = +(request.params as SetParams).set_id;

        return reply.view("config/credential-form", {
            action: "Create",
            set_id,
            data: { username: "" },
        });
    }

    async createHandle(request: FastifyRequest, reply: FastifyReply) {
        const set_id = +(request.params as SetParams).set_id;
        const body = request.body as Record<string, unknown>;

        const parsed = parseCredentialBody({ ...body, set_id });
        if (!parsed.success) {
            return reply.code(400).view("config/credential-form", {
                action: "Create",
                set_id,
                data: body,
                error: zodErr(parsed.error),
            });
        }

        const pass = credentialPassword.safeParse(body.password ?? "");
        if (!pass.success) {
            return reply.code(400).view("config/credential-form", {
                action: "Create",
                set_id,
                data: body,
                error: zodErr(pass.error),
            });
        }

        await db.insert(smtpCredentials).values({
            ...parsed.data,
            hash: await hashPassword(pass.data),
        });
        return reply.redirect(`/config/credentials/${set_id}`);
    }

    async edit(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(smtpCredentials)
            .where(eq(smtpCredentials.id, id))
            .limit(1);
        if (!data) {
            return reply.redirect("/config/credentials");
        }

        return reply.view("config/credential-form", {
            action: "Update",
            set_id: data.set_id,
            data,
        });
    }

    async editHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;
        const body = request.body as Record<string, unknown>;

        const [existing] = await db
            .select()
            .from(smtpCredentials)
            .where(eq(smtpCredentials.id, id))
            .limit(1);
        if (!existing) {
            return reply.redirect("/config/credentials");
        }

        const parsed = parseCredentialBody({
            ...body,
            set_id: existing.set_id,
        });
        if (!parsed.success) {
            return reply.code(400).view("config/credential-form", {
                action: "Update",
                set_id: existing.set_id,
                data: { ...body, id },
                error: zodErr(parsed.error),
            });
        }

        // "Leave blank to keep": the form never echoes a hash back, so a blank
        // password means "rename the user, keep the credential" rather than
        // "set the password to nothing".
        const values: { username: string; set_id: number; hash?: string } = {
            ...parsed.data,
        };
        const supplied = String(body.password ?? "");
        if (supplied !== "") {
            const pass = credentialPassword.safeParse(supplied);
            if (!pass.success) {
                return reply.code(400).view("config/credential-form", {
                    action: "Update",
                    set_id: existing.set_id,
                    data: { ...body, id },
                    error: zodErr(pass.error),
                });
            }
            values.hash = await hashPassword(pass.data);
        }

        await db
            .update(smtpCredentials)
            .set(values)
            .where(eq(smtpCredentials.id, id));
        return reply.redirect(`/config/credentials/${existing.set_id}`);
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(smtpCredentials)
            .where(eq(smtpCredentials.id, id))
            .limit(1);

        return reply.view("config/credential-delete", { data });
    }

    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(smtpCredentials)
            .where(eq(smtpCredentials.id, id))
            .limit(1);
        const set_id = data?.set_id;
        await db.delete(smtpCredentials).where(eq(smtpCredentials.id, id));

        return reply.redirect(`/config/credentials/${set_id}`);
    }
}

async function hashPassword(pass: string): Promise<string> {
    const salt = await bcrypt.genSalt(BCRYPT_COST);
    return bcrypt.hash(pass, salt);
}
