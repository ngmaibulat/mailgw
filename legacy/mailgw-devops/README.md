### Prerequisites

-   Install latest Ubuntu Server LTS
-   Install packages:
    -   docker.io
    -   swaks
    -   vim
    -   tcpdump
-   Pull containers
-   Allow 25/tcp and 2525/tcp from approved senders
-   Create directories: /opt/mailgw/config, /opt/mailgw/queue, /opt/mailgw/logs
-   Use `--restart=unless-stopped` on all containers
-   Use `--network=host` on mailgw

### Restart Option

```bash
docker update --restart=unless-stopped mailgw
```
