import { express } from "../adapter.js";
import * as functions from "../functions.mjs";

const router = express.Router();

router.post("/md5", (req, res) => {
    console.log(req.body);

    functions
        .hashListLookup(req.body)
        .then((result) => {
            console.log(result);
            return result;
        })
        .then((result) => {
            res.send({ action: result });
        });
});

export default router;
