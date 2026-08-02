import bcrypt from "bcryptjs";
import sequelize from "../config/sequelize.mjs";
import User from "../esmmodels/user.mjs";

const email = "admin@example.com";
const pass = "P@ssw0rd";
const salt = await bcrypt.genSalt(10);
const hash = await bcrypt.hash(pass, salt);

const item = await User.create({ email, hash });

const records = await User.findAll();

console.log(records);
sequelize.close();
