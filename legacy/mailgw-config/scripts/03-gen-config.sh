### Generate configs

mkdir /opt/mailgw
sudo chown $USER:$USER /opt/mailgw -R

pnpm install
node dist/index.js
