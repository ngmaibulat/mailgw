const models = require("../models");

async function corsConfig() {
    const whitelist = [];

    function checkOrigin(origin, callback) {
        if (!whitelist.length) {
            //no origins are defined
            //allow
            callback(null, true);
        } else if (whitelist.indexOf(origin) !== -1) {
            callback(null, true);
        } else {
            callback(new Error("Not allowed by CORS"));
        }
    }

    const query = {
        where: {
            name: "cors_origins",
        },
    };

    const origins = await models.Config.findAll(query);
    console.log("Origins:");
    console.log(origins);

    if (!origins.length) {
        console.warn("No CORS Origins are defined in the Config Database");
        console.warn("Defaulting to allow any Origin");
        console.warn(
            "Please define the actual CORS origins -- the domain names where the frontend is hosted"
        );
    } else {
        //copy values from the model to whitelist
    }

    const corsOptions = {
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

    return corsOptions;
}

// export default corsOptions;

module.exports = corsConfig;
