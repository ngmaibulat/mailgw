#!/bin/sh

# env

mysql --protocol TCP -h $DB_HOST -u$DB_USER -p$DB_PASS $DB_NAME
