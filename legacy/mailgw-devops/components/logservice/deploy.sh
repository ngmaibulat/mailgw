export dbname=mailgw
export dbuser=mailgw
export dbpass=P@ssw0rd

mkdir -p /opt/mailgw


### Create .env file
cat << EOF > /opt/.env
NODE_ENV=production
PORT=3000

DB_DRIVER=mysql
DB_HOST=db
DB_NAME=$dbname
DB_USER=$dbuser
DB_PASS=$dbpass
EOF


### Pull containers
docker pull mysql:latest
docker pull ngmaibulat/db-migrator:latest
docker pull ngmaibulat/logservice:latest

### Create network and volume
docker network create --driver bridge --subnet 10.0.0.0/24 mailgw
docker volume create data

### Run containers
docker run
        --name db \
        --restart=unless-stopped \
        --network $dbname \
        -p 3306:3306 \
        -v data:/var/lib/mysql \
        -e MYSQL_USER=$dbuser \
        -e MYSQL_PASSWORD=$dbpass \
        -e MYSQL_ROOT_PASSWORD=$dbpass \
        -e MYSQL_DATABASE=$dbname \
        -d mysql:latest

sleep 60

docker run -it --rm --network=$dbname --env-file /opt/.env ngmaibulat/db-migrator:latest

docker run
        --name logservice \
        --restart=unless-stopped \
        --network=$dbname \
        --env-file /opt/.env \
        -p 3000:3000 \
        -d ngmaibulat/logservice:latest
