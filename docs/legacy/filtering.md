can we do the following API calls? The json definitely has to be URL-encoded? Do we need to decode that ourselves? Would Bun.serve decode itself? New endpoints:

- GET /api/delivery?q=<json>
- GET /api/connection?q=<json>
- GET /api/transaction?q=<json>
  Example query: ?q={"limit":50,"offset":0,"search":[{"field":"sender","operator":"contains","value":"@gmail.com"}],"searchLogic":"AND"}

---

Good questions. Let me address each:

**Does the JSON need to be URL-encoded?**

Yes, it must be — the `{`, `"`, `:` characters in JSON are not valid raw in a query string. So the client encodes:

```
?q=%7B%22limit%22%3A50%2C%22offset%22%3A0%2C...%7D
```

**Does Bun.serve decode it automatically?**

Yes — `new URL(req.url).searchParams.get("q")` gives you the already-decoded string. You just `JSON.parse()` it. No manual `decodeURIComponent` needed.

Here's a clean pattern for all three endpoints:

```typescript
const server = Bun.serve({
  port: 3000,
  async fetch(req) {
    const url = new URL(req.url);
    const path = url.pathname;

    // Parse ?q= param (Bun/URL API decodes it automatically)
    const parseQuery = (url: URL) => {
      const raw = url.searchParams.get("q");
      if (!raw) return null;
      try {
        return JSON.parse(raw); // raw is already URL-decoded
      } catch {
        return null;
      }
    };

    if (req.method === "GET" && path === "/api/delivery") {
      const q = parseQuery(url);
      if (!q) return new Response("Invalid or missing ?q=", { status: 400 });
      // use q.limit, q.offset, q.search, q.searchLogic ...
      return Response.json({ endpoint: "delivery", query: q });
    }

    if (req.method === "GET" && path === "/api/connection") {
      const q = parseQuery(url);
      if (!q) return new Response("Invalid or missing ?q=", { status: 400 });
      return Response.json({ endpoint: "connection", query: q });
    }

    if (req.method === "GET" && path === "/api/transaction") {
      const q = parseQuery(url);
      if (!q) return new Response("Invalid or missing ?q=", { status: 400 });
      return Response.json({ endpoint: "transaction", query: q });
    }

    return new Response("Not Found", { status: 404 });
  },
});
```

**On the client side**, encoding is just:

```typescript
const q = encodeURIComponent(JSON.stringify({ limit: 50, offset: 0, search: [...], searchLogic: "AND" }));
const res = await fetch(`/api/delivery?q=${q}`);
```

**One thing to consider:** if your `search` array can get large, the URL can exceed browser/server limits (~8KB in most cases). If that's a risk, switching to `POST /api/delivery` with a JSON body is safer and cleaner. Worth keeping in mind.

---

Exactly — the schema you sketched is already quite capable. You can layer on a lot without changing the wire format:

**What you already have:**

```json
{ "field": "sender", "operator": "contains", "value": "@gmail.com" }
```

**Operators you can support per field type:**

| Type        | Operators                                                           |
| ----------- | ------------------------------------------------------------------- |
| string      | `contains`, `not_contains`, `starts_with`, `ends_with`, `eq`, `neq` |
| number/date | `eq`, `neq`, `gt`, `gte`, `lt`, `lte`, `between`                    |
| boolean     | `eq`                                                                |
| array/enum  | `in`, `not_in`                                                      |

**Nested logic** — AND within OR groups, for free:

```json
{
  "search": [
    { "field": "sender", "operator": "contains", "value": "@gmail.com" },
    { "field": "status", "operator": "in", "value": ["delivered", "pending"] },
    { "field": "amount", "operator": "between", "value": [100, 500] }
  ],
  "searchLogic": "AND"
}
```

**Sorting + pagination is a natural addition:**

```json
{
  "limit": 50,
  "offset": 0,
  "sort": [{ "field": "created_at", "direction": "desc" }],
  "search": [...],
  "searchLogic": "AND"
}
```

**The backend then just maps this to SQL/ORM conditions** — one `parseFilters(q.search)` function that walks the array and builds your WHERE clause. The three endpoints (`delivery`, `connection`, `transaction`) can all share that exact same parser, only differing in which table/model they query against.

It essentially gives you a lightweight query language over HTTP — similar to what tools like PostgREST or Supabase expose, but fully under your control.
