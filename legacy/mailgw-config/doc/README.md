### D.O. VM

```bash
doctl compute droplet create \
    --image ubuntu-23-04-x64 \
    --size s-1vcpu-2gb \
    --region fra1 \
    --vpc-uuid d02044c0-1c69-4da7-ad45-27fe9a2ceaf6 \
    --ssh-keys 37590350 \
    srv-m-01
```

### Clone

```bash
git clone https://github.com/ngmaibulat/mailgw-config.git
cd mailgw-config
rm -fr .git
```

### DotEnv

```bash
cp example.env .env
vim .env
```

### Run scripts

```bash
bash scripts/01-packages.sh
bash scripts/02-node.sh

### Check configs
# autogenerate json configs
# put them to destination folder
bash scripts/03-gen-config.sh
bash scripts/04-pull-image.sh

### [PM2][ERROR] Process or Namespace mailgw not found
bash scripts/05-run-container.sh

bash scripts/06-dnat.sh
bash scripts/07-test.sh
bash scripts/08-pm2.sh
```

### Test

From `another` host

```bash
nc -vz host 2525
swaks -s host -f addr@domain -t addr@domain
```
