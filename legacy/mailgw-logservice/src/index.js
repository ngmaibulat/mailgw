const express = require("express");
const dotenv = require("dotenv");

const routes = require("./routes/root.js");
const routesApi = require("./routes/api.js");

const models = require("../models");

dotenv.config();
const app = express();
app.set("x-powered-by", false);

app.use(express.urlencoded({ extended: true }));
app.use(express.json());

app.use("/", routes);
app.use("/api", routesApi);

app.use((req, res, next) => {
    res.status(404).send("Resourse does not exist\n");
    console.error("404 not found:", req.path);
});

initModelLinks(models);

function initModelLinks(models) {
    models.MailAddr.belongsToMany(models.Transaction, {
        through: "linkAddrTransaction",
    });
    models.Transaction.belongsToMany(models.MailAddr, {
        through: "linkAddrTransaction",
    });
}

try {
    const port = process.env.PORT || 3000;

    app.listen(port);

    console.log("[app] mode: " + app.get("env"));
    console.log(`[app] port: ${port}`);
} catch (err) {
    console.error(err);
    //also record error to Database
}

process.on("unhandledRejection", (reason, promise) => {
    console.error("Unhandled Rejection at:", promise, "reason:", reason);
    // Application specific logging, throwing an error, or other logic here
});
