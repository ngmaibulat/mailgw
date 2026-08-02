$(function () {
    // https://w2ui.com/web/docs/1.5/w2utils.settings
    // https://github.com/vitmalina/w2ui/blob/d91c5dee807e10bf20bff82282413d43a2297ac6/docs/details/w2utils.settings.html

    (w2utils.settings.dateFormat = "yyyy-mm-dd"),
        (w2utils.settings.timeFormat = "hh:mi:ss"),
        (w2utils.settings.datetimeFormat = "yyyy-mm-dd hh:mi:ss"),
        (w2utils.settings.datetimeFormat = "yyyy-mm-dd"),
        (w2utils.settings.date_display = "yyyy-mm-dd"),
        $("#grid").w2grid({
            name: "grid",
            recid: "id",
            url: "/api/delivery",
            method: "GET", // need this to avoid 412 error on Safari
            multiSearch: true,

            show: {
                header: true,
                footer: true,
                toolbar: true,
                lineNumbers: true,
            },

            searches: [
                // { type: 'int',  field: 'recid', label: 'ID' },
                // { type: 'date', field: 'dt', label: 'Date' },
                { type: "datetime", field: "dt", label: "DateTime" },
                { type: "text", field: "sender", label: "Sender" },
                { type: "text", field: "rcpt_list", label: "Recipients" },
                { type: "text", field: "rcpt_domain", label: "Rcpt Domain" },
                {
                    type: "text",
                    field: "rcpt_accepted",
                    label: "Rcpt Accepted",
                },
                { type: "text", field: "tls_forced", label: "TLS Enforced" },
                { type: "text", field: "tls", label: "TLS" },
                { type: "text", field: "auth", label: "Auth" },
                { type: "text", field: "host", label: "Host" },
                { type: "text", field: "ip", label: "IP" },
                { type: "text", field: "port", label: "Port" },
                { type: "text", field: "response", label: "Reponse" },
                { type: "text", field: "delay", label: "Delay" },
            ],

            columns: [
                { field: "dt", text: "DT", render: "datetime" },
                { field: "uuid", text: "UUID" },

                { field: "sender", text: "Sender" },
                { field: "rcpt_list", text: "Rcpt" },

                // { field: 'dt', text: 'DT', size: '30%' },

                { field: "rcpt_domain", text: "Rcpt Domain" },
                { field: "rcpt_accepted", text: "Rcpt Accepted" },
                { field: "tls_forced", text: "TLS Enforced" },
                { field: "tls", text: "TLS" },
                { field: "auth", text: "Auth" },
                { field: "host", text: "Host" },
                { field: "ip", text: "IP" },
                { field: "port", text: "Port" },
                { field: "response", text: "Reponse" },
                { field: "delay", text: "Delay" },
            ],
        });

    w2ui.grid.hideColumn("uuid");
    w2ui.grid.hideColumn("rcpt_domain");
    w2ui.grid.hideColumn("rcpt_accepted");
    w2ui.grid.hideColumn("tls_forced");
    w2ui.grid.hideColumn("tls");
    w2ui.grid.hideColumn("auth");
    w2ui.grid.hideColumn("delay");
});
