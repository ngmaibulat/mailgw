### PM2 tasks

```bash
cd /opt/logservice
npm install -g pm2
pm2 start 'npm run start' -n logservice
pm2 save
pm2 ls
pm2 startup
```

### PM2 Daemon

Once you have run `pm2 startup`
and run the command it generated
PM2 should run automatically after reboot and should `resurrect` you services

Test that:

```bash
sudo reboot

#after rebbot
sudo systemctl status pm2-$USER
pm2 ls
```
