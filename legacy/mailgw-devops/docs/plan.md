### Plan

- pull docker containers
- push docker compose yaml files: `docker-compose.yml`
- run all containers
- test tcp ports

### Other Containers

- Portainer
- Prometheus
- Grafana
- Vault
- Nexus?
- Gitlab?
- Minio?

### Host

- Firewall rules
- Node Exporter
- Docker Exporter
- Docker Logs: archive somewhere
- Mount SMB3 share

### Prometheus in Docker Compose

```yaml
version: "3.8"

services:
  prometheus:
    image: prom/prometheus
    volumes:
      - /path/to/prometheus.yml:/etc/prometheus/prometheus.yml
    network_mode: "host"
```

### Prometheus Config

```yaml
scrape_configs:
  - job_name: "node"
    static_configs:
      - targets: ["localhost:9100"]
```

### Sample - using Docker Compose

```yaml
- name: Deploy a compose file
  community.docker.docker_compose:
    project_src: /path/to/your/docker-compose.yml
    state: present
```

### Sample - wait for TCP port

```yaml
- name: Wait for the database to be ready
  wait_for:
    host: "{{ db_host }}"
    port: 5432
    delay: 10 # Optional initial delay in seconds
    timeout: 60 # Maximum time in seconds to wait
```

### Docker Compose Sample

```yaml
---
version: "3.8"

x-environment: &common-env
  DB_HOST: db
  DB_USER: user
  DB_PASSWORD: password
  DB_NAME: mydb

services:
  db:
    image: mysql:8.2
    environment:
      MYSQL_ROOT_PASSWORD: rootpassword
      MYSQL_DATABASE: "mailgw"
      MYSQL_USER: "mailgw"
      MYSQL_PASSWORD: "somepassword"
    ports:
      - "3306:3306"
    volumes:
      - db_data:/var/lib/mysql
    restart: on-failure

  run-migrations:
    image: migration-image
    depends_on:
      - db
    environment:
      <<: *common-env
    # No restart policy, or could be 'restart: "no"'

  log-service:
    image: log-service-image
    depends_on:
      - db
      - run-migrations
    environment:
      <<: *common-env
    volumes:
      - "/var/run/docker.sock:/var/run/docker.sock"
      - "/opt/logs:/opt/logs"
    restart: on-failure

  web-ui:
    image: web-ui-image
    ports:
      - "8080:8080"
    depends_on:
      - db
    environment:
      <<: *common-env
    restart: on-failure

volumes:
  db_data:
```
