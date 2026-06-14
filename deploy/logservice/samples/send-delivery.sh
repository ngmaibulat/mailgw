#!/bin/bash

export URL="http://localhost:3000/api/delivery"

curl -X POST -H "Content-Type: application/json" -d @delivery/delivery.json $URL

curl -X POST -H "Content-Type: application/json" -d @delivery/delivery-1.bad $URL

curl -X POST -H "Content-Type: application/json" -d @delivery/delivery-2.bad $URL
