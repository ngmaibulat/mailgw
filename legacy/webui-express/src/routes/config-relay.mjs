import { express } from "../adapter.js";
import { CtrlRelayGroup } from "../controllers/CtrlRelayGroup.js";
import { CtrlRelay } from "../controllers/CtrlRelay.js";

const router = express.Router();

let ctrlRelayGroup = new CtrlRelayGroup();
let ctrlRelay = new CtrlRelay();

///////////////////////////////////////////////////////

router.get("/relay/create/:group_id", ctrlRelay.create);
router.post("/relay/create/:group_id", ctrlRelay.createHandle);

router.get("/relay/edit/:id", ctrlRelay.edit);
router.post("/relay/edit/:id", ctrlRelay.editHandle);

router.get("/relay/delete/:id", ctrlRelay.delete);
router.post("/relay/delete/:id", ctrlRelay.deleteHandle);

///////////////////////////////////////////////////////

router.get("/relaygrp/create", ctrlRelayGroup.create);
router.post("/relaygrp/create", ctrlRelayGroup.createHandle);

router.get("/relaygrp/edit/:id", ctrlRelayGroup.edit);
router.post("/relaygrp/edit/:id", ctrlRelayGroup.editHandle);

router.get("/relaygrp/delete/:id", ctrlRelayGroup.delete);
router.post("/relaygrp/delete/:id", ctrlRelayGroup.deleteHandle);

router.get("/relaygrp/:id", ctrlRelayGroup.details);
router.get("/relaygrp", ctrlRelayGroup.index);

///////////////////////////////////////////////////////

router.get("/routing", (req, res) => {
    res.render("util/notimpl", {});
});

export default router;
