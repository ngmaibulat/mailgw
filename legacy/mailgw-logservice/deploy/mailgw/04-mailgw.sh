### Pull container

docker pull docker.io/ngmaibulat/mailgw:latest

### Run container

docker run --name mailgw \
 --net=host \
 --mount type=bind,source=/opt/mailgw/config,target=/opt/mailgw/config \
 --mount type=bind,source=/opt/mailgw/queue,target=/opt/mailgw/queue \
 --mount type=bind,source=/opt/mailgw/log,target=/opt/mailgw/log \
 -d ngmaibulat/mailgw
