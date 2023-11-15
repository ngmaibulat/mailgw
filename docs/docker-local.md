### Run locally via Docker

```bash
docker pull docker.io/ngmaibulat/mailgw

docker run --name mailgw \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -p 2525:2525 \
 -d ngmaibulat/mailgw
```

### Run locally via Podman

```bash
podman pull docker.io/ngmaibulat/mailgw

podman run --name mailgw \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -p 2525:2525 \
 -d ngmaibulat/mailgw
```
