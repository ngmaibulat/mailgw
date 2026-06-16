import { zod as u } from "../adapter.ts";

export const AuthInfo = u.object({
    email: u.string().min(4).max(50).email(),
    pass: u.string().min(8).max(20),
});
