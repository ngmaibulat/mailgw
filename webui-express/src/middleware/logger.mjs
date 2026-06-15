import Log from "../../db/esmmodels/log.mjs";

export function logger(req, res, next) {
    const logdata = {};
    logdata.url = req.protocol + "://" + req.get("host") + req.originalUrl;
    logdata.path = req.path;
    logdata.query = req.originalUrl;
    logdata.method = req.method;
    logdata.contentype = req.get("Content-Type");
    logdata.protocol = req.protocol;
    logdata.src_ip = req.ip;
    logdata.src_port = req.socket.remotePort;
    logdata.referer = req.header("Referrer") || req.header("Referer");
    logdata.userAgent = req.header("User-agent");
    logdata.origin = req.header("Origin");
    logdata.user = "-";

    try {
        Log.create(logdata);
    } catch (err) {
        console.error(err);
    }

    // Log.create(logdata);

    if (process.env.NODE_ENV == "development") {
        console.log(logdata);
    }

    next();
}
