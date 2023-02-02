FROM node:18

RUN mkdir -p /opt/mailgw/plugins

COPY package.json /opt/mailgw/
COPY pnpm-lock.yaml  /opt/mailgw/
COPY plugins  /opt/mailgw/plugins

WORKDIR /opt/mailgw

RUN npm i -g pnpm
RUN pnpm install

EXPOSE 8080

CMD [ "npm", "run", "start" ]
