
### Stop Previous Container

pm2 stop mailgw
podman stop mailgw
podman rm mailgw

### Host Net

podman run --name mailgw \
 --net=host \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -d ngmaibulat/mailgw


### Default Net

podman run --name mailgw \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -p 2525:2525 \
 -d ngmaibulat/mailgw

echo ""
echo ""

podman ps

export host=`hostname`
echo "See containers in web ui: https://$host:9090/podman"
