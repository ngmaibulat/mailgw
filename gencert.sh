#!/bin/bash

# config/tls_cert.pem
# config/tls_key.pem

mkcert localhost

mv localhost.pem      config/tls_cert.pem
mv localhost-key.pem  config/tls_key.pem
