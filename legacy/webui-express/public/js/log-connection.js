$(function () {
    $("#grid").w2grid({
        name: "grid",
        recid: "id",
        url: "/api/connection",
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
            { type: "datetime", field: "dt", label: "DT" },
            { type: "text", field: "uuid", label: "UUID" },
        ],

        columns: [
            { field: "dt", text: "DT" },
            { field: "uuid", text: "UUID" },

            { field: "encoding", text: "Encoding" },
            { field: "hello_name", text: "Src HELO" },
            { field: "remoteAddr", text: "Src Addr" },
            { field: "remotePort", text: "Src Port" },

            { field: "remote_host", text: "Src Host" },
            { field: "remote_info", text: "Src Info" },

            { field: "remote_is_local", text: "isLocal" },
            { field: "remote_is_private", text: "isPrivate" },
            { field: "using_tls", text: "TLS" },

            { field: "tran_count", text: "Transaction Count" },
            { field: "rcpt_count_accept", text: "Rcpt Accepted" },
            { field: "rcpt_count_tempfail", text: "Rcpt Templ Fail" },
            { field: "rcpt_count_reject", text: "Rcpt Rejected" },
        ],
    });

    w2ui.grid.hideColumn("uuid");
    w2ui.grid.hideColumn("encoding");

    w2ui.grid.hideColumn("remotePort");
    w2ui.grid.hideColumn("remote_host");
    w2ui.grid.hideColumn("remote_info");
    w2ui.grid.hideColumn("remote_is_local");

    w2ui.grid.hideColumn("rcpt_count_tempfail");
    w2ui.grid.hideColumn("rcpt_count_reject");
});
