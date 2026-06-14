const functions = require("./functions");

const logfile = "./log/log-connection.log";

exports.hook_connect = function (next, connection) {
    const obj = functions.buildConnInfo(connection);

    functions.log(functions.toJson(obj), logfile);

    // Placeholder for IP allow/deny policy. Real allowlisting lives in npFilter.
    if (isBlacklistedIP(obj.remoteAddr)) {
        return next(DENY, "Denied by IP blacklist");
    }

    return next();
};

function isBlacklistedIP(ip) {
    // Implement your logic to check if an IP is blacklisted
    return false;
}
