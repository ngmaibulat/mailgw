import db from "../db";

export interface ConnectionRow {
    uuid: string;
    dt: number;
    encoding: string | null;
    hello_name: string | null;
    remoteAddr: string;
    remotePort: number;
    remote_host: string | null;
    remote_info: string | null;
    remote_is_local: boolean;
    remote_is_private: boolean;
    using_tls: boolean;
    tran_count: number;
    rcpt_count_accept: number;
    rcpt_count_tempfail: number;
    rcpt_count_reject: number;
}

export async function insertConnection(c: ConnectionRow): Promise<void> {
    await db`
        INSERT INTO Connection
            (uuid, dt, encoding, hello_name, remoteAddr, remotePort,
             remote_host, remote_info, remote_is_local, remote_is_private,
             using_tls, tran_count, rcpt_count_accept, rcpt_count_tempfail,
             rcpt_count_reject, createdAt, updatedAt)
        VALUES
            (${c.uuid}, FROM_UNIXTIME(${c.dt} / 1000), ${c.encoding}, ${c.hello_name}, ${c.remoteAddr},
             ${c.remotePort}, ${c.remote_host}, ${c.remote_info}, ${c.remote_is_local},
             ${c.remote_is_private}, ${c.using_tls}, ${c.tran_count},
             ${c.rcpt_count_accept}, ${c.rcpt_count_tempfail}, ${c.rcpt_count_reject},
             NOW(), NOW())
    `;
}
