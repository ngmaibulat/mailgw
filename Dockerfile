FROM node:20

RUN mkdir -p /opt/mailgw/plugins

COPY package.json /opt/mailgw/
COPY pnpm-lock.yaml  /opt/mailgw/
COPY plugins  /opt/mailgw/plugins

WORKDIR /opt/mailgw

RUN npm i -g pnpm
RUN pnpm install

RUN apt update
RUN apt upgrade -y
RUN apt install -y iproute2 net-tools inetutils-ping tcpdump curl vim dnsutils swaks

EXPOSE 8080

CMD [ "npm", "run", "start" ]
