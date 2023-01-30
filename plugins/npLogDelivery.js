const fs = require("fs");
// const fetch = require("node-fetch");
const functions = require("./functions");

const logfile = "./log/logDelivery.log";

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

    functions.httplog(logdata, url_delivery).then((response) => {
        const tm = Date.now();
        const dt = new Date(tm);
        const logTime = dt.toISOString();

        const logdata = {
            tm: logTime,
            status: response.status,
            statusText: response.statusText,
        };

        functions.log(JSON.stringify(logdata), logfile);
    });
    // httplog(JSON.stringify("+++++++++++++++++++++++++"))
    // httplog(params);

    return next();
};

exports.register = function () {
    // this.register_hook('delivered', 'hook_delivered');
};