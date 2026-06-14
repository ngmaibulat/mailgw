### Common

- download/unpack
- move to /opt
- create database
- run migrations
- make env file

### Prepare Package

```bash
npm install
npx sequelize-cli db:migrate
npm run start
```

### Link to new Version

```bash
pm2 stop logservice # if we have pm2 task
rm -f /opt/logservice
sudo ln -s /opt/logservice-version /opt/logservice
sudo chown $USER:$USER . -R
pm2 start logservice # if we have pm2 task
```
