import sequelize from "./db/config/sequelize.mjs";
import { checkAuth } from "./src/auth/util.mjs";

if (process.argv.length < 4) {
    console.error("Usage node check-user.mjs <username> <password>");
    process.exit(1);
}

const email = process.argv[2];
const pass = process.argv[3];

console.log(email, pass);

const authResult = await checkAuth(email, pass);

if (!authResult) {
    console.error("Auth failed");
} else {
    console.log("Auth success");
}

sequelize.close();
