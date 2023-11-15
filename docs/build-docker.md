### Build

```bash
docker build . -t ngmaibulat/mailgw
```

### Push

```bash
docker tag ngmaibulat/mailgw ngmaibulat/mailgw:v0.0.15
docker push ngmaibulat/mailgw
```

### Podman

```bash
podman login docker.io
podman build . -t ngmaibulat/mailgw

podman tag localhost/ngmaibulat/mailgw docker.io/ngmaibulat/mailgw:v0.0.14
podman push ngmaibulat/mailgw
```
