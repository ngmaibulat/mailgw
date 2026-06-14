# logservice-bun API

## Base URL

```
http://localhost:3000
```

---

## GET /

Health check.

```
GET /
```

**Response**
```json
{ "status": "OK" }
```

---

## POST /api/delivery

Record a delivery event from the Haraka `hook_delivered` plugin.

```
POST /api/delivery
Content-Type: application/json
```

**Request body**
```json
{
    "uuid": "abc-123",
    "dt": 1717833600,
    "sender": "user@example.com",
    "rcpt_domain": "gmail.com",
    "rcpt_list": "recipient@gmail.com",
    "rcpt_accepted": "recipient@gmail.com",
    "tls_forced": false,
    "tls": true,
    "auth": false,
    "host": "smtp.gmail.com",
    "ip": "74.125.0.1",
    "port": "25",
    "response": "250 OK",
    "delay": 1.23
}
```

**Response**
```json
{ "status": "OK" }
```

**Error (validation failure)**
```json
{ "status": "Fail" }
```
HTTP 400

---

## POST /api/connection

Record a connection event from the Haraka `hook_connect` plugin.

```
POST /api/connection
Content-Type: application/json
```

**Request body**
```json
{
    "uuid": "abc-123",
    "dt": 1717833600,
    "encoding": "utf8",
    "hello_name": "mail.example.com",
    "remoteAddr": "10.0.0.1",
    "remotePort": 54321,
    "remote_host": "mail.example.com",
    "remote_info": "Postfix",
    "remote_is_local": false,
    "remote_is_private": true,
    "using_tls": true,
    "tran_count": 1,
    "rcpt_count_accept": 1,
    "rcpt_count_tempfail": 0,
    "rcpt_count_reject": 0
}
```

**Response**
```json
{ "status": "OK" }
```

---

## POST /api/queue

Record a queue event from the Haraka `hook_queue_outbound` plugin.

```
POST /api/queue
Content-Type: application/json
```

**Request body** — same shape as `/api/connection`

**Response**
```json
{ "status": "OK" }
```

---

## GET /api/delivery

Search delivery records.

```
GET /api/delivery?q=<json>
```

**Query parameter `q`** (JSON-encoded):

| Field         | Type   | Description                        |
|---------------|--------|------------------------------------|
| `limit`       | number | Max records to return (default 100)|
| `offset`      | number | Pagination offset (default 0)      |
| `searchLogic` | string | `"AND"` or `"OR"` (default `"AND"`)  |
| `search`      | array  | Array of filter objects (see below)|

**Filter object:**

| Field      | Type   | Description                                    |
|------------|--------|------------------------------------------------|
| `field`    | string | Column name (see allowed fields below)         |
| `operator` | string | One of the operators below                     |
| `value`    | any    | Filter value (`[v1, v2]` for `between`)        |

**Allowed fields:** `id`, `uuid`, `dt`, `sender`, `rcpt_domain`, `rcpt_list`,
`rcpt_accepted`, `tls_forced`, `tls`, `auth`, `host`, `ip`, `port`, `response`, `delay`

**Operators:**

| Operator   | SQL equivalent         |
|------------|------------------------|
| `is` / `=` | `= ?`                  |
| `begins`   | `LIKE 'val%'`          |
| `contains` | `LIKE '%val%'`         |
| `ends`     | `LIKE '%val'`          |
| `between`  | `BETWEEN ? AND ?`      |
| `>` / `more` | `> ?`              |
| `>=`       | `>= ?`                 |
| `<` / `less` | `< ?`              |
| `<=`       | `<= ?`                 |

**Examples**

Search by sender domain:
```
GET /api/delivery?q={"limit":50,"offset":0,"searchLogic":"AND","search":[{"field":"sender","operator":"contains","value":"@gmail.com"}]}
```

Search by TLS and date range:
```
GET /api/delivery?q={"limit":100,"offset":0,"searchLogic":"AND","search":[{"field":"tls","operator":"=","value":1},{"field":"dt","operator":"between","value":[1717833600,1717920000]}]}
```

**Response**
```json
{
    "status": "success",
    "total": 2,
    "records": [
        {
            "id": 42,
            "uuid": "abc-123",
            "dt": 1717833600,
            "sender": "user@gmail.com",
            ...
        }
    ]
}
```

---

## GET /api/connection

Search connection records.

```
GET /api/connection?q=<json>
```

Same query format as `/api/delivery`.

**Allowed fields:** `id`, `uuid`, `dt`, `encoding`, `hello_name`, `remoteAddr`,
`remotePort`, `remote_host`, `remote_info`, `remote_is_local`, `remote_is_private`,
`using_tls`, `tran_count`, `rcpt_count_accept`, `rcpt_count_tempfail`, `rcpt_count_reject`

**Example** — find all connections from a specific IP:
```
GET /api/connection?q={"search":[{"field":"remoteAddr","operator":"is","value":"10.0.0.1"}]}
```

---

## GET /api/transaction

Search transaction records.

```
GET /api/transaction?q=<json>
```

Same query format as `/api/delivery`.

**Allowed fields:** `id`, `uuid`, `dt`, `action`, `encoding`, `sender`, `rcpt_list`,
`rcpt_count_accept`, `rcpt_count_tempfail`, `rcpt_count_reject`,
`delay_data_post`, `data_bytes`, `mime_part_count`

**Example** — find large messages:
```
GET /api/transaction?q={"search":[{"field":"data_bytes","operator":">","value":1000000}]}
```
