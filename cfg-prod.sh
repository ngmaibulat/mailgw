#!/bin/bash

node scripts/config.mjs

npx envsub --env-file .env templates/routing.json config/routing.json
npx envsub --env-file .env templates/relays.json config/relays.json
