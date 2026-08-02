### Build

```bash
docker build -t ngmaibulat/logservice .
docker tag ngmaibulat/logservice ngmaibulat/logservice:latest
```

### Network

```bash
docker network create mailgw
```

### Database

- launch MySQL
- create .env file
- run migrations

```bash
docker pull mysql
docker run --network mailgw -e MYSQL_ROOT_PASSWORD=P@ssw0rd -d --name db mysql
docker exec -it db mysql -uroot -pP@ssw0rd

docker run -e MYSQL_ROOT_PASSWORD=P@ssw0rd -p 3306:3306 -d --name dblocal mysql
mysql --protocol TCP -h localhost -uroot -pP@ssw0rd
```

### Run

```bash
cat .env

docker pull ngmaibulat/logservice
docker run --network mailgw --env-file .env -p 3000:3000 -d --name logservice ngmaibulat/logservice

docker exec -it logservice /bin/sh
echo ping -c 4 db | docker exec -i logservice /bin/sh
curl http://localhost:3000
```
