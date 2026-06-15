import { express, bodyParser, cookieParser } from "./adapter.js";

import { checkSession } from "./middleware/checkSession.mjs";
import { logger } from "./middleware/logger.mjs";

import { login } from "./auth/login.mjs";
import { logout } from "./auth/logout.mjs";
import { profile } from "./auth/profile.mjs";
import { formLogin } from "./forms/formLogin.mjs";

import routes from "./routes/root.mjs";
import routesLog from "./routes/log.mjs";
import routesApi from "./routes/api.mjs";
import routesFilter from "./routes/filter.mjs";
import relayConfig from "./routes/config-relay.mjs";

import { initModelLinks } from "../db/config/links.mjs";
initModelLinks();

const app = express();

const template_engine = process.env.TEMPLATE_ENGINE || "pug"; //pug | ejs
const template_dir = process.env.TEMPLATE_DIR || "./templates/pug";

//Settings
app.set("view engine", template_engine);
app.set("views", template_dir);
app.set("appName", "NGM Mail Router");
app.set("x-powered-by", false);

//Middlewares
const cookieSecret = process.env.SIGN_COOKIE || "sign";
app.use(express.static("./public"));
app.use(bodyParser.urlencoded({ extended: true }));
app.use(bodyParser.json());
app.use(cookieParser(cookieSecret));
app.use(logger);

//Auth
app.get("/login", formLogin);
app.post("/login", login);
app.get("/logout", logout);
app.get("/profile", profile);
app.use(checkSession);

//Routes
app.use("/", routes);
app.use("/log", routesLog);
app.use("/api", routesApi);
app.use("/filter", routesFilter);
app.use("/config", relayConfig);

export { app };
