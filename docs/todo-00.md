### Common

- docker
- test on kubernetes
- test on cloud run
- tls
- ip filter
- configurator: me, host_list: set from a json file
- dkim
- plugins in separate packages

### Initial Run Script

- get env and generate configs
- use dotenv package, so if env vars are not set - they would be loaded from .env
- get tls certs from env and save to files

### Container

- initial run script
- kubernetes
- test config env
- test secrets: tls

### TLS

- load from env and save to file
- use Kuber Secrets

### Logger

- NPM package for logging plugin
- load configs from env: destination url, etc
