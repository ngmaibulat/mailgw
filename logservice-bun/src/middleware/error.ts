type Handler = (req: Request) => Response | Promise<Response>;

export function withErrorHandling(handler: Handler): Handler {
    return async (req: Request) => {
        try {
            return await handler(req);
        } catch (err) {
            const url = new URL(req.url);
            console.error(`[error] ${req.method} ${url.pathname}:`, err);
            return Response.json({ status: "Error", message: "Internal server error" }, { status: 500 });
        }
    };
}
