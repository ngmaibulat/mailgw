import { bcrypt } from "../adapter.js";
import User from "../../db/esmmodels/user.mjs";

export async function checkAuth(email, pass) {
    const record = await User.findOne({
        where: {
            email,
        },
    });

    if (!record) {
        console.error(`User not found: ${email}`);
        return false;
    }

    const res = bcrypt.compareSync(pass, record.hash);
    return res;
}
