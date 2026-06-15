import { express } from "../adapter.js";
const router = express.Router();

router.get("/", (req, res) => {
    // res.json(delivery);
    res.render("index", {});
});

export default router;
