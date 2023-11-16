const fs = require("fs");
// const fetch = require("node-fetch");
const functions = require("./functions");

const version = "0.0.21";

const logfile = "./log/log-delivery.log";

exports.getConfig = function () {
    const path = "logging.json";
    const config = this.config.get(path, "json");
    return config;
};

exports.hook_delivered = function (next, hmail, params) {
    const config = exports.getConfig();
    const url_delivery = config.url_delivery;

    functions.log(JSON.stringify(config), logfile);

    let host = params[0];
    let ip = params[1];
    let response = params[2];
    let delay = params[3];
    let port = params[4];
    let mode = params[5];
    let ok_recips = params[6];
    let secured = params[7];
    let authenticated = params[8];

    let logdata = {
        uuid: hmail.todo.uuid,
        dt: hmail.todo.queue_time,
        sender: functions.getAddr(hmail.todo.mail_from),
        rcpt_domain: hmail.todo.domain,
        rcpt_list: functions.getAddrList(hmail.todo.rcpt_to),
        rcpt_accepted: functions.getAddrList(ok_recips),
        tls_forced: hmail.force_tls,
        tls: secured,
        auth: authenticated,
        // todo: hmail.todo,
        host: host,
        ip: ip,
        port: port,
        response: response,
        delay: delay,
        // params: params
    };

    functions
        .httplog(logdata, url_delivery)
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
                    logdata: logdata,
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
                logdata: logdata,
            };

            functions.log(JSON.stringify(errorLogData), logfile);
        });

    return next();
};

exports.register = function () {
    // this.register_hook("delivered", "hook_delivered");
};
