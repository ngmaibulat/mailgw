### Env vars

-   copy `app.env` to `.env`
-   update

### Run

```bash
docker-compose up -d
```

### Check MySQL

```bash
docker-compose exec db mysql -u root -p
```

### Migrations

```bash
docker-compose -f docker-compose.migrate.yaml up
```
