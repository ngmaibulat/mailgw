#!/bin/bash

if test -f ".env"; then
    echo ".env file exists: continue work"
else
    echo "file not found: .env"
    echo "please create it or link it via"
    echo -e "\t ln -s target .env"
    echo "exiting..."
    exit
fi

rm -fr config
rm -fr queue
rm -fr log
rm -fr node_modules

pnpm install

node scripts/config.mjs

npx envsub --env-file .env templates/routing.json config/routing.json
npx envsub --env-file .env templates/relays.json config/relays.json
npx envsub --env-file .env templates/logging.json config/logging.json

mkcert localhost

mv localhost.pem      config/tls_cert.pem
mv localhost-key.pem  config/tls_key.pem
