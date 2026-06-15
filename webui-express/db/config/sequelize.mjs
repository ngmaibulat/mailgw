import { Sequelize } from "../../src/adapter.js";
import config from "./config.mjs";

const sequelize = new Sequelize(
    config.database,
    config.username,
    config.password,
    config
);

try {
    await sequelize.authenticate();
    console.log("DB Connection: OK");
} catch (error) {
    console.error("DB Connection: FAIL");
    process.exit(1);
}

//check schema

try {
    const tables = await sequelize.query("show tables");

    const countTables = tables[0].length;
    if (countTables < 15) {
        console.error("\tError: schema is not created");
        console.error("\tPlease run database migrations in logservice package");
        process.exit(0);
    }

    console.log(tables[0]);
} catch (error) {
    console.error("Error getting list of tables");
    process.exit(1);
}

export default sequelize;
