const functions = require("./functions");

const logfile = "./log/log-delivery.log";

exports.getConfig = function () {
    return this.config.get("logging.json", "json");
};

exports.hook_delivered = function (next, hmail, params) {
    const config = exports.getConfig.call(this);
    const url_delivery = config.url_delivery;

    functions.log(JSON.stringify(config), logfile);

    let host = params[0];
    let ip = params[1];
    let response = params[2];
    let delay = params[3];
    let port = params[4];
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
        host: host,
        ip: ip,
        port: port,
        response: response,
        delay: delay,
    };

    functions.postWithLogging(logdata, url_delivery, logfile);

    return next();
};
