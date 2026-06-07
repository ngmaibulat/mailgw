### Env file

Make sure to have /opt/logservice.env
Example content:

```bash
NODE_ENV="production"
PORT=3000

DB_DRIVER="mysql"
DB_HOST="127.0.0.1"
DB_NAME="mailgw"
DB_USER="root"
DB_PASS="P@ssw0rd"
```

In version's folder, link .env to /opt/logservice.env

```bash
cd /opt/logservice-version
ln -s ../logservice.env .env
```
