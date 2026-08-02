### Env File: /opt/.env

```bash
NODE_ENV=production
PORT=3000

DB_DRIVER=mysql
DB_HOST=localhost
DB_NAME=mailgw
DB_USER=root
DB_PASS=P@ssw0rd
```

### Load Env File in Bash

```bash
source /opt/.env
echo $DB_DRIVER
```
