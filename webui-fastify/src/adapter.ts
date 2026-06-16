// Shared third-party imports. After the Drizzle migration the data layer no
// longer goes through here (see db/index.ts); this is just the small set of
// utilities a few modules share. (zod is imported directly from "zod/v4" where
// needed — see src/validation/.)
import { v4 as uuidv4 } from "uuid";
import bcrypt from "bcryptjs";

export { uuidv4, bcrypt };
