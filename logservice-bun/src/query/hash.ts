import { findBlockedMD5s } from "../models/blockmd5";
import { insertHashLookup } from "../models/hashlookup";

export type Action = "allow" | "block";

// Attachment descriptor as POSTed by the Haraka npFilterAttach plugin.
export interface Attachment {
    md5?: string | null;
    contentType?: string | null;
    filename?: string | null;
    size?: number | null;
    txn_uuid?: string | null;
}

export interface AttachmentDecision extends Attachment {
    action: Action;
}

// Pure decision: given the set of blocked md5s, classify each attachment and
// the overall message action. The message blocks if ANY attachment is blocked.
export function decideActions(
    list: Attachment[],
    blocked: Set<string>,
): { overall: Action; decisions: AttachmentDecision[] } {
    let overall: Action = "allow";
    const decisions = list.map((a) => {
        const action: Action = a.md5 != null && blocked.has(a.md5) ? "block" : "allow";
        if (action === "block") overall = "block";
        return { ...a, action };
    });
    return { overall, decisions };
}

// Look up each attachment's md5 against the blocklist, record the lookups in
// HashLookups, and return the overall action for the message.
export async function hashListLookup(list: Attachment[]): Promise<Action> {
    const md5s = list
        .map((a) => a.md5)
        .filter((m): m is string => m != null && m !== "");

    const blockedRows = await findBlockedMD5s(md5s);
    const blocked = new Set(blockedRows.map((r) => r.md5));

    const { overall, decisions } = decideActions(list, blocked);

    await Promise.all(
        decisions.map((d) =>
            insertHashLookup({
                txn_uuid: d.txn_uuid ?? "",
                md5: d.md5 ?? "",
                contentType: d.contentType ?? null,
                filename: d.filename ?? null,
                size: d.size ?? null,
                action: d.action,
            }),
        ),
    );

    return overall;
}
