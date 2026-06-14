type Handler = (req: Request) => Response | Promise<Response>;

const API_KEY = Bun.env.API_KEY;

if (!API_KEY) {
    console.warn("[auth] API_KEY not set — all requests will be accepted");
}

export function withAuth(handler: Handler): Handler {
    return async (req: Request) => {
        if (API_KEY && req.headers.get("X-API-Key") !== API_KEY) {
            return Response.json({ status: "Unauthorized" }, { status: 401 });
        }
        return handler(req);
    };
}
