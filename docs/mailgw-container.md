### Check dirs

-   /opt/mailgw/config
-   /opt/mailgw/queue
-   /opt/mailgw/log

### Check configs

```
ls -la /opt/mailgw/config
```

### Run

```bash
docker-compose up -d
```

### Test

```bash
swaks -s 127.0.0.1:25 -t test@example.com -f test@example.com
```
