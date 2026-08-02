### Load env vars
source /opt/.env

export dbname=$DB_NAME
export dbuser=$DB_USER
export dbpass=$DB_PASS
export port=$PORT

### Pull containers
docker pull mysql:latest
docker pull ngmaibulat/db-migrator:latest
docker pull ngmaibulat/logservice:latest

### Run migrations to update schema
docker run -it --rm --network=$dbname --env-file /opt/.env ngmaibulat/db-migrator:latest

### Remove old logservice container
docker stop logservice
docker rm logservice

### Run new logservice container
docker run -d --network=$dbname --env-file /opt/.env -p 3000:$port --name logservice ngmaibulat/logservice:latest

### Test
curl -X POST -H "Content-Type: application/json" -d @samples/connection/conn.json http://localhost:3000/api/connection
curl -X POST -H "Content-Type: application/json" -d @samples/delivery/delivery.json http://localhost:3000/api/delivery
