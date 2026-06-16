const url = new URL(window.location);
const usp = url.searchParams;

const msg = usp.get("msg");

// Per-`?msg=` alert text. Anything unrecognised falls back to the generic
// invalid-credentials message (the form's default alert text).
const messages = {
    InvalidAuth: "Invalid username or password",
    ValidationError: "Please enter a valid email and password",
    TooManyAttempts: "Too many login attempts. Please wait a minute and try again.",
};

if (msg !== null) {
    const el = document.getElementById("alert");
    el.textContent = messages[msg] ?? messages.InvalidAuth;
    el.style.display = "block";
}
