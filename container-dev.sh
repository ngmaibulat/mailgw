#!/bin/bash

docker stop dev-mailgw

docker rm -f $(docker ps -aq)

docker run -d \
  --name dev-mailgw \
  -v ${PWD}/plugins:/opt/mailgw/plugins \
  -v /opt/mailgw/config:/opt/mailgw/config \
  -v /opt/mailgw/log:/opt/mailgw/log \
  -v /opt/mailgw/queue:/opt/mailgw/queue \
  -p 25:25 \
  ngmaibulat/mailgw:latest
