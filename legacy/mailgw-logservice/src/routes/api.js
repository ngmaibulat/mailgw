const express = require("express");
const router = express.Router();

const models = require("../../models");
const { schemaDelivery } = require("../validation/delivery");

module.exports = router;

router.post("/delivery", (req, res) => {
    const parsed = schemaDelivery.safeParse(req.body);

    if (parsed.success) {
        models.Delivery.create(req.body);
        res.send('{"status": "OK"}\n');
    } else {
        res.status(400).send('{"status": "Fail"}\n');
        console.error(parsed.error.issues);
    }

    // console.log(req.body);
});

router.post("/queue", (req, res) => {
    // models.Connection.create(req.body);
    console.log(req.body);
    res.send('{"status": "OK"}\n');
});

router.post("/connection", (req, res) => {
    // models.Connection.create(req.body);
    console.log(req.body);
    res.send('{"status": "OK"}\n');
});
