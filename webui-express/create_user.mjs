import bcrypt from "bcryptjs";
import sequelize from "./db/config/sequelize.mjs";
import User from "./db/esmmodels/user.mjs";

if (process.argv.length < 4) {
    console.error("Usage node create-users.mjs <username> <password>");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];

console.log(email, pass);

const salt = await bcrypt.genSalt(10);
const hash = await bcrypt.hash(pass, salt);

try {
    const item = await User.create({ email, hash });
    console.log("User created!");
} catch (err) {
    console.error("Error creating user");
}

sequelize.close();
