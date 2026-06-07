FROM node:26-alpine

# https://pkgs.alpinelinux.org/packages?name=python3&branch=v3.18&repo=&arch=&maintainer=
# RUN apk add build-base

RUN apk add --no-cache python3 py3-pip make g++

RUN mkdir -p /opt/mailgw/plugins

COPY package.json /opt/mailgw/
COPY pnpm-lock.yaml  /opt/mailgw/
COPY plugins  /opt/mailgw/plugins

WORKDIR /opt/mailgw

# RUN npm i -g pnpm
RUN npm install -g pnpm@11

#RUN pnpm install
RUN pnpm install --prod --frozen-lockfile

# RUN apt update
# RUN apt upgrade -y
# RUN apt install -y iproute2 net-tools inetutils-ping tcpdump curl vim dnsutils swaks

EXPOSE 25

CMD [ "npm", "run", "start" ]
