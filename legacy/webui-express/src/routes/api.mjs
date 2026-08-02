import { express } from "../adapter.js";
import * as functions from "../functions.mjs";
const router = express.Router();

// const models = require("../../db/models");

import { Mail } from "../../db/rmodels/Mail.mjs";
import { Delivery } from "../../db/esmmodels/delivery.mjs";
import { Connection } from "../../db/esmmodels/connection.mjs";
import { Transaction } from "../../db/esmmodels/transaction.mjs";
import { v_lookup } from "../../db/esmmodels/v_lookup.mjs";

router.post("/delivery", (req, res) => {
    Delivery.create(req.body);
    // console.log(req.body);
    res.send("OK: ");
});

router.get("/delivery", (req, res) => {
    functions
        .getData(Delivery, req.query.request)
        .then(functions.prepareRes)
        .then((resp) => res.json(resp));
});

router.post("/connection", (req, res) => {
    Connection.create(req.body);
    console.log(req.body);
    res.send("OK");
});

router.get("/connection", (req, res) => {
    // res.set('Access-Control-Allow-Origin', '*');

    functions
        .getData(Connection, req.query.request)
        .then(functions.prepareRes)
        .then((resp) => res.json(resp));
});

router.post("/queue", (req, res) => {
    // console.log(req.body);

    let m = new Mail(req.body);
    m.save();
    res.send("OK");
});

router.get("/queue", (req, res) => {
    functions
        .getData(Transaction, req.query.request)
        .then(functions.prepareRes)
        .then((resp) => res.json(resp));
});

router.get("/hashlookups", (req, res) => {
    functions
        .getData(v_lookup, req.query.request)
        .then(functions.prepareRes)
        .then((resp) => res.json(resp));
});

export default router;
