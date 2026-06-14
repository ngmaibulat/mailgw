FROM node:26-alpine

# https://pkgs.alpinelinux.org/packages?name=python3&branch=v3.18&repo=&arch=&maintainer=
# RUN apk add build-base

RUN apk add --no-cache python3 py3-pip make g++

RUN mkdir -p /opt/mailgw/plugins

# Build context is the repo root (see containers/container-build.sh) so the
# workspace lockfile is visible. The image keeps a flat /opt/mailgw layout, so
# runtime mounts and the container path contract are unchanged.
COPY mailgw/package.json /opt/mailgw/package.json
COPY pnpm-lock.yaml  /opt/mailgw/
COPY mailgw/plugins  /opt/mailgw/plugins

WORKDIR /opt/mailgw

# RUN npm i -g pnpm
RUN npm install -g pnpm@11

# Single-package prod build: the workspace lockfile doesn't apply standalone,
# so we don't use --frozen-lockfile here.
RUN pnpm install --prod

# RUN apt update
# RUN apt upgrade -y
# RUN apt install -y iproute2 net-tools inetutils-ping tcpdump curl vim dnsutils swaks

EXPOSE 25

CMD [ "npm", "run", "start" ]
