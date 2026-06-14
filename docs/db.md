```sql
CREATE USER 'mailgw'@'localhost' IDENTIFIED BY 'P@ssw0rd';
GRANT ALL PRIVILEGES ON mailgw.* TO 'mailgw'@'localhost';
FLUSH PRIVILEGES;
```

```bash
mariadb -u mailgw -p -e "CREATE DATABASE mailgw;"
```
