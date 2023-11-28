const fs = require("fs");
// const fetch = require("node-fetch");
const functions = require("./functions");

const version = "0.0.21";
const logfile = "./log/log-data.log";

exports.getConfig = function () {
    const path = "logging.json";
    const config = this.config.get(path, "json");
    return config;
};

exports.hook_data = function (next, connection) {
    let obj = {
        uuid: connection.uuid,
        dt: connection.start_time,

        // always should be == 5 for hook_data
        state: connection.state,

        encoding: connection.encoding,
        remoteAddr: connection.client.remoteAddress,
        remotePort: connection.client.remotePort,

        remote_is_local: connection.remote_is_local,
        remote_is_private: connection.remote_is_private,
        // remote_ip: connection.remote_ip,
        // remote_port: connection.remote_port,
        remote_host: connection.remote_host,
        remote_info: connection.remote_info,

        // cfg: connection.cfg,
        // local: connection.local,
        // remote: connection.remote,

        // hello: connection.hello,
        hello_name: connection.hello.host,
        // tls: connection.tls,
        // proxy: connection.proxy,
        using_tls: connection.using_tls,

        /////////////////////////////

        // notes: connection.notes,

        //this one fails for json encoding or being empty
        // transaction: connection.transaction,

        tran_count: connection.tran_count,

        // our config - no need to log
        // ehlo_hello_message: connection.ehlo_hello_message,

        // our config - no need to log
        // connection_close_message: connection.connection_close_message,

        // our config - no need to log
        // banner_includes_uuid: connection.banner_includes_uuid,
        // deny_includes_uuid: connection.deny_includes_uuid,

        // not sure what it is:
        // early_talker: connection.early_talker,
        pipelining: connection.pipelining,

        // not really usefull info:
        // esmtp: connection.esmtp,

        rcpt_count_accept: connection.rcpt_count.accept,
        rcpt_count_tempfail: connection.rcpt_count.tempfail,
        rcpt_count_reject: connection.rcpt_count.reject,

        //not yet having this info or can be misleading:
        // msg_count: connection.msg_count,

        // ignore as of now:
        // errors: connection.errors,
        // last_rcpt_msg: connection.last_rcpt_msg,
    };

    functions.log(functions.toJson(obj), logfile);

    const config = exports.getConfig();
    const url = config.url_conn;

    functions
        .httplog(obj, url)
        .then((response) => {
            const tm = Date.now();
            const dt = new Date(tm);
            const logTime = dt.toISOString();

            if (response && response.status) {
                const logSuccess = {
                    tm: logTime,
                    version: version,
                    status: response.status,
                    statusText: response.statusText,
                };

                functions.log(JSON.stringify(logSuccess), logfile);
            } else {
                // Handle cases where response is undefined, null, or missing a status
                const errorLogData = {
                    tm: logTime,
                    version: version,
                    error: "HTTP Logfail, please review logs on Logger side",
                    logdata: obj,
                };

                functions.log(JSON.stringify(errorLogData), logfile);
            }
        })
        .catch((error) => {
            const tm = Date.now();
            const dt = new Date(tm);
            const logTime = dt.toISOString();

            const errorLogData = {
                tm: logTime,
                version: version,
                error: "HTTP Connect Error",
                httperror: error,
                logdata: obj,
            };
            functions.log(JSON.stringify(errorLogData), logfile);
        });

    return next();
};
