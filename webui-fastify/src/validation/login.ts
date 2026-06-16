import { z } from "zod/v4";

export const AuthInfo = z.object({
    email: z.email().min(4).max(50),
    pass: z.string().min(8).max(20),
});

// First-run admin creation (src/auth/setup.ts). Same email/password rules as
// login, plus a confirmation field so a typo can't silently lock out the only
// account that setup can ever create.
export const SetupInfo = z
    .object({
        email: z.email().min(4).max(50),
        pass: z.string().min(8).max(20),
        passConfirm: z.string(),
    })
    .refine((data) => data.pass === data.passConfirm, {
        message: "Passwords do not match",
        path: ["passConfirm"],
    });

// Change-password form on /profile. `current` is the existing password (re-auth
// before a change); `pass`/`passConfirm` are the new one, same rules as login.
export const ChangePassword = z
    .object({
        current: z.string().min(8).max(20),
        pass: z.string().min(8).max(20),
        passConfirm: z.string(),
    })
    .refine((data) => data.pass === data.passConfirm, {
        message: "New passwords do not match",
        path: ["passConfirm"],
    });

// Admin user management (src/controllers/CtrlUser.ts). Create requires a
// password; edit allows leaving it blank to keep the current one.
export const UserCreate = z.object({
    email: z.email().min(4).max(50),
    pass: z.string().min(8).max(20),
});

export const UserEdit = z.object({
    email: z.email().min(4).max(50),
    // "" = keep the current password; otherwise the same 8–20 rule as login.
    pass: z
        .string()
        .max(20)
        .refine((v) => v === "" || v.length >= 8, {
            message:
                "Password must be 8–20 characters (or blank to keep current)",
        }),
});
