import { express } from "../adapter.js";
const router = express.Router();

router.get("/delivery", (req, res) => {
    res.render("log-delivery", {});
});

router.get("/connection", (req, res) => {
    res.render("log-connection", {});
});

router.get("/mails", (req, res) => {
    res.render("log-mails", {});
});

router.get("/lookups", (req, res) => {
    res.render("log-lookups", {});
});

export default router;
