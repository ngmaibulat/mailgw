docker run -it --rm --network=host --env-file .env ngmaibulat/db-migrator:latest

docker compose run --rm db-migrator
