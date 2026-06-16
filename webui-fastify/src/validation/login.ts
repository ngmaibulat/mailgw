import { z } from "zod/v4";

export const AuthInfo = z.object({
    email: z.email().min(4).max(50),
    pass: z.string().min(8).max(20),
});
