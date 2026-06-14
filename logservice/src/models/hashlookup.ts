import db from "../db";

export interface HashLookupRow {
    txn_uuid: string;
    md5: string;
    contentType: string | null;
    filename: string | null;
    size: number | null;
    action: "allow" | "block";
}

export async function insertHashLookup(row: HashLookupRow): Promise<void> {
    await db`
        INSERT INTO HashLookups
            (txn_uuid, md5, contentType, filename, size, action, createdAt, updatedAt)
        VALUES
            (${row.txn_uuid}, ${row.md5}, ${row.contentType}, ${row.filename},
             ${row.size}, ${row.action}, NOW(), NOW())
    `;
}
