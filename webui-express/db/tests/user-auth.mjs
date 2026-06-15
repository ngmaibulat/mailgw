import bcrypt from "bcryptjs";
import sequelize from "../config/sequelize.mjs";
import User from "../esmmodels/user.mjs";

const email = "admin@example.com";
const pass = "P@ssw0rd";

const record = await User.findOne({
    where: {
        email,
    },
});

console.log(record.hash);

const res = bcrypt.compareSync(pass, record.hash);
console.log(res);

sequelize.close();
