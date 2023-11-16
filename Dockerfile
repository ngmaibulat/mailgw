FROM node:20-alpine

# https://pkgs.alpinelinux.org/packages?name=python3&branch=v3.18&repo=&arch=&maintainer=
RUN apk add python3
RUN apk add py3-pip
RUN apk add make
RUN apk add g++
# RUN apk add build-base

RUN mkdir -p /opt/mailgw/plugins

COPY package.json /opt/mailgw/
COPY pnpm-lock.yaml  /opt/mailgw/
COPY plugins  /opt/mailgw/plugins

WORKDIR /opt/mailgw

RUN npm i -g pnpm
RUN pnpm install

# RUN apt update
# RUN apt upgrade -y
# RUN apt install -y iproute2 net-tools inetutils-ping tcpdump curl vim dnsutils swaks

EXPOSE 25

CMD [ "npm", "run", "start" ]
