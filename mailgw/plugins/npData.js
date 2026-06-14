const functions = require("./functions");

const logfile = "./log/log-data.log";

exports.getConfig = function () {
    return this.config.get("logging.json", "json");
};

exports.hook_data = function (next, connection) {
    const obj = functions.buildConnInfo(connection);
    const url = exports.getConfig.call(this).url_conn;

    functions.postWithLogging(obj, url, logfile);

    return next();
};
