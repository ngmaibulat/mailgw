### Interactive Run

```bash
export name=mailgw-interactive
export dir=`pwd`

docker run --name $name \
 --mount type=bind,source=$dir/config,target=/opt/mailgw/config \
 -it ngmaibulat/mailgw

docker run --name $name \
 --mount type=bind,source=$dir/config,target=/opt/mailgw/config \
 -it ngmaibulat/mailgw /bin/bash

docker start -ai $name

docker rm $name
```

### Daemon Mode

docker run --name mailgw \
 --mount type=bind,source=$dir/config,target=/opt/mailgw/config \
 -d ngmaibulat/mailgw
