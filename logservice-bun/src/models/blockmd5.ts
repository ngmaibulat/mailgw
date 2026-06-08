import db from "../db";

export interface BlockMD5Row {
    id: number;
    md5: string;
    comment: string | null;
    createdAt: Date;
    updatedAt: Date;
}

export async function findBlockedMD5s(md5s: string[]): Promise<BlockMD5Row[]> {
    if (md5s.length === 0) return [];
    return db`SELECT * FROM BlockMD5s WHERE md5 IN ${db(md5s)}` as Promise<BlockMD5Row[]>;
}

export async function isMD5Blocked(md5: string): Promise<boolean> {
    const rows = await db`SELECT id FROM BlockMD5s WHERE md5 = ${md5} LIMIT 1` as BlockMD5Row[];
    return rows.length > 0;
}
