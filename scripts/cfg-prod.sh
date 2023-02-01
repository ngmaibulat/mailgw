#!/bin/bash

pnpm install

if test -f ".env"; then
    echo ".env file exists: continue work"
else
    echo "file not found: .env"
    echo "please create it or link it via"
    echo -e "\t ln -s target .env"
    echo "exiting..."
    exit
fi

node scripts/config.mjs


if test -f "/opt/routing.json"; then
    echo "config exists: /opt/routing.json"
    echo "skip generating..."
else
    npx envsub --env-file .env templates/routing.json /opt/routing.json
fi


if test -f "/opt/relays.json"; then
    echo "config exists: /opt/relays.json"
    echo "skip generating..."
else
    npx envsub --env-file .env templates/relays.json /opt/relays.json
fi


if test -f "/opt/logging.json"; then
    echo "config exists: /opt/logging.json"
    echo "skip generating..."
else
    npx envsub --env-file .env templates/logging.json /opt/logging.json
fi

echo ""
echo "Linking json config files to config folder"
echo ""

ln -s /opt/routing.json config/routing.json
ln -s /opt/relays.json config/relays.json
ln -s /opt/logging.json config/logging.json
