const functions = require("./functions");

const logfile = "./log/log-queue.log";

exports.getConfig = function () {
    return this.config.get("logging.json", "json");
};

exports.hook_queue_outbound = function (next, connection) {
    const obj = functions.buildConnInfo(connection);
    const url = exports.getConfig.call(this).url_queue;

    functions.postWithLogging(obj, url, logfile);

    return next();
};
