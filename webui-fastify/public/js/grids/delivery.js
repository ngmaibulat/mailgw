// Delivery log grid (Delivery table via /api/delivery).
LogGrid.create({
    url: "/api/delivery",
    columns: [
        LogGrid.dt("dt", "DT"),
        LogGrid.text("gateway", "Gateway", { maxWidth: 160 }),
        LogGrid.text("sender", "Sender"),
        LogGrid.text("rcpt_list", "Rcpt"),
        LogGrid.text("host", "Host"),
        LogGrid.text("ip", "IP"),
        LogGrid.num("port", "Port", { maxWidth: 110 }),
        LogGrid.text("response", "Response"),
        // Hidden by default — toggle from the column menu.
        LogGrid.text("uuid", "UUID", { hide: true, minWidth: 200 }),
        // Which routing rule sent this recipient here. Empty when the default
        // action applied.
        LogGrid.text("route_rule", "Route rule", { hide: true, minWidth: 200 }),
        LogGrid.text("rcpt_domain", "Rcpt Domain", { hide: true }),
        LogGrid.text("rcpt_accepted", "Rcpt Accepted", { hide: true }),
        LogGrid.text("tls_forced", "TLS Enforced", { hide: true }),
        LogGrid.text("tls", "TLS", { hide: true }),
        LogGrid.text("auth", "Auth", { hide: true }),
        LogGrid.num("delay", "Delay", { hide: true }),
    ],
});
