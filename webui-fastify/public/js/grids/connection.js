// Connection log grid (Connection table via /api/connection).
LogGrid.create({
    url: "/api/connection",
    columns: [
        LogGrid.dt("dt", "DT"),
        LogGrid.text("hello_name", "Src HELO"),
        LogGrid.text("remoteAddr", "Src Addr"),
        LogGrid.text("remote_is_private", "isPrivate", { maxWidth: 120 }),
        LogGrid.text("using_tls", "TLS", { maxWidth: 100 }),
        LogGrid.num("tran_count", "Txns", { maxWidth: 110 }),
        LogGrid.num("rcpt_count_accept", "Rcpt OK", { maxWidth: 120 }),
        // Hidden by default — toggle from the column menu.
        LogGrid.text("uuid", "UUID", { hide: true, minWidth: 200 }),
        LogGrid.text("encoding", "Encoding", { hide: true }),
        LogGrid.num("remotePort", "Src Port", { hide: true }),
        LogGrid.text("remote_host", "Src Host", { hide: true }),
        LogGrid.text("remote_info", "Src Info", { hide: true }),
        LogGrid.text("remote_is_local", "isLocal", { hide: true }),
        LogGrid.num("rcpt_count_tempfail", "Rcpt Tempfail", { hide: true }),
        LogGrid.num("rcpt_count_reject", "Rcpt Rejected", { hide: true }),
    ],
});
