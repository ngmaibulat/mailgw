### Prepare

-   ensure having /opt/mailgw with the following subfolders: config, queue, log
-   /opt/mailgw/config should have working config

### Pull latest image

```bash
podman pull docker.io/ngmaibulat/mailgw
podman images
```

### Stop/delete container

```bash
pm2 stop mailgw
podman stop mailgw
podman rm mailgw
```

### Create/run container

-   create/check pm2 task for container

```
podman run --name mailgw \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -p 2525:2525 \
 -d ngmaibulat/mailgw

pm2 start mailgw
pm2 save
```
