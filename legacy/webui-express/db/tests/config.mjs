import sequelize from "../config/sequelize.mjs";
import Config from "../esmmodels/config.mjs";

const item = await Config.create({ name: "ip", value: "127.0.0.1" });

const records = await Config.findAll();

console.log(records);
sequelize.close();
