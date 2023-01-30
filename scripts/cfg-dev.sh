#!/bin/bash

rm -fr config
rm -fr queue
rm -fr log

pnpm install

node scripts/config.mjs

npx envsub --env-file .env templates/routing.json config/routing.json
npx envsub --env-file .env templates/relays.json config/relays.json
npx envsub --env-file .env templates/logging.json config/logging.json

mkcert localhost

mv localhost.pem      config/tls_cert.pem
mv localhost-key.pem  config/tls_key.pem