#!/bin/bash

### Check directory /opt/mailgw
ls -l /opt/ | grep -q "mailgw"

if [ $? -ne 0 ]; then
    echo "Directory /opt/mailgw not found"
    exit 1
fi

### Check directory /opt/mailgw/config
ls -l /opt/mailgw/ | grep -q "config"

if [ $? -ne 0 ]; then
    echo "Directory /opt/mailgw/config not found"
    exit 1
fi

### Check directory /opt/mailgw/log
ls -l /opt/mailgw/ | grep -q "log"

if [ $? -ne 0 ]; then
    echo "Directory /opt/mailgw/log not found"
    exit 1
fi

### Check directory /opt/mailgw/queue
ls -l /opt/mailgw/ | grep -q "queue"

if [ $? -ne 0 ]; then
    echo "Directory /opt/mailgw/queue not found"
    exit 1
fi
