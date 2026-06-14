#!/bin/bash

### script builds containers
### pushes them to docker hub


export version=0.0.7

### Build
docker build . -f containers/logservice/Dockerfile -t ngmaibulat/logservice
docker build . -f containers/mysql-cli/Dockerfile -t ngmaibulat/mysql-cli
docker build . -f containers/db-migrator/Dockerfile -t ngmaibulat/db-migrator
docker build . -f containers/env/Dockerfile -t ngmaibulat/env

### Tag
docker tag ngmaibulat/logservice   ngmaibulat/logservice:$version
docker tag ngmaibulat/logservice   ngmaibulat/logservice:latest

docker tag ngmaibulat/mysql-cli   ngmaibulat/mysql-cli:$version
docker tag ngmaibulat/mysql-cli   ngmaibulat/mysql-cli:latest

docker tag ngmaibulat/db-migrator ngmaibulat/db-migrator:$version
docker tag ngmaibulat/db-migrator ngmaibulat/db-migrator:latest

docker tag ngmaibulat/env         ngmaibulat/env:$version
docker tag ngmaibulat/env         ngmaibulat/env:latest

### Push
docker push ngmaibulat/logservice:$version
docker push ngmaibulat/logservice:latest

docker push ngmaibulat/mysql-cli:$version
docker push ngmaibulat/mysql-cli:latest

docker push ngmaibulat/db-migrator:$version
docker push ngmaibulat/db-migrator:latest

docker push ngmaibulat/env:$version
docker push ngmaibulat/env:latest

### Test Locally
docker run -it --rm --network=host --env-file .env ngmaibulat/db-migrator:$version
# docker run -it --rm --network=host --env-file .env ngmaibulat/mysql-cli:$version
# docker run -it --rm --network=host --env-file .env ngmaibulat/env:$version

# LogService install
# docker pull ngmaibulat/logservice:latest
# docker run -d --network=host --env-file /opt/.env -p 3000:3000 --name logservice ngmaibulat/logservice:latest

alias mig='docker run -it --rm --network=host --env-file .env ngmaibulat/db-migrator:$version npx sequelize-cli'

mig db:migrate:status
