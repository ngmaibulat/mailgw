function corsConfig() {
    function checkOrigin(origin, callback) {
        if (!origin) {
            //this must be same origoin request
            //must be...
            callback(null, true);
        } else if (!corsOptions.whitelist.length) {
            //no origins are defined
            //allow
            callback(null, true);
        } else if (corsOptions.whitelist.indexOf(origin) !== -1) {
            callback(null, true);
        } else {
            callback(new Error("Not allowed by CORS"));
        }
    }

    const corsOptions = {
        whitelist: [],
        origin: checkOrigin,
        // origin: "http://localhost:3002",
        methods: ["GET", "HEAD", "PUT", "PATCH", "POST", "DELETE"],
        allowedHeaders: ["Content-Type", "Authorization"],
        exposedHeaders: ["Content-Type", "Authorization"],
        credentials: false,
        maxAge: 600,

        preflightContinue: false,
        optionsSuccessStatus: 204,
    };

    const query = {
        where: {
            name: "cors_origins",
        },
    };

    const origins = process.env.CORS_ORIGINS;
    const methods = process.env.CORS_METHODS;
    const headers = process.env.CORS_HEADERS;
    const maxAge = process.env.CORS_CACHE_SECONDS;
    const useCreds = process.env.CORS_CREDS;

    if (!origins || origins == "*") {
        console.warn("No CORS Origins are defined");
        console.warn("Defaulting to allow any Origin");
        console.warn(
            "Please define the actual CORS origins -- the domain names where the frontend is hosted"
        );
    } else {
        //copy values from the model to whitelist
        corsOptions.whitelist = origins.split(",");
        corsOptions.methods = methods.split(",");
        corsOptions.allowedHeaders = headers.split(",");
        corsOptions.exposedHeaders = headers.split(",");
        corsOptions.credentials = useCreds ? true : false;
        corsOptions.maxAge = +maxAge;
    }

    return corsOptions;
}

module.exports = corsConfig;
