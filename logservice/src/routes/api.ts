import { schemaDelivery } from "../validation/delivery";
import { insertDelivery } from "../models/delivery";
import { insertConnection } from "../models/connection";
import { searchDelivery, searchConnection, searchTransaction } from "../query/search";
import { hashListLookup } from "../query/hash";
import type { ConnectionRow } from "../models/connection";

export async function deliveryRoute(req: Request): Promise<Response> {
    const body = await req.json();
    const parsed = schemaDelivery.safeParse(body);

    if (!parsed.success) {
        console.error(parsed.error.issues);
        return Response.json({ status: "Fail" }, { status: 400 });
    }

    await insertDelivery(parsed.data);
    return Response.json({ status: "OK" });
}

export async function deliverySearchRoute(req: Request): Promise<Response> {
    const q = new URL(req.url).searchParams.get("q");
    const result = await searchDelivery(q);
    return Response.json(result);
}

export async function queueRoute(req: Request): Promise<Response> {
    const body = await req.json();
    await insertConnection(toConnectionRow(body));
    return Response.json({ status: "OK" });
}

export async function connectionRoute(req: Request): Promise<Response> {
    const body = await req.json();
    await insertConnection(toConnectionRow(body));
    return Response.json({ status: "OK" });
}

function toConnectionRow(body: any): ConnectionRow {
    return {
        uuid:                body.uuid         ?? null,
        dt:                  body.dt           ?? null,
        encoding:            body.encoding     ?? null,
        hello_name:          body.hello_name   ?? null,
        remoteAddr:          body.remoteAddr   ?? null,
        remotePort:          body.remotePort   ?? null,
        remote_host:         body.remote_host  ?? null,
        remote_info:         body.remote_info  ?? null,
        remote_is_local:     body.remote_is_local    ?? false,
        remote_is_private:   body.remote_is_private  ?? false,
        using_tls:           body.using_tls    ?? false,
        tran_count:          body.tran_count   ?? 0,
        rcpt_count_accept:   body.rcpt_count_accept   ?? 0,
        rcpt_count_tempfail: body.rcpt_count_tempfail ?? 0,
        rcpt_count_reject:   body.rcpt_count_reject   ?? 0,
    };
}

export async function connectionSearchRoute(req: Request): Promise<Response> {
    const q = new URL(req.url).searchParams.get("q");
    const result = await searchConnection(q);
    return Response.json(result);
}

// Attachment MD5 blocklist check. The Haraka npFilterAttach plugin POSTs an
// array of attachment descriptors and expects { action: "allow" | "block" }.
export async function filterMD5Route(req: Request): Promise<Response> {
    const body = await req.json();
    const list = Array.isArray(body) ? body : [];
    const action = await hashListLookup(list);
    return Response.json({ action });
}

export async function transactionSearchRoute(req: Request): Promise<Response> {
    const q = new URL(req.url).searchParams.get("q");
    const result = await searchTransaction(q);
    return Response.json(result);
}
