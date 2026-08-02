
pm2 start 'podman start -a mailgw' -n mailgw

pm2 save

pm2 startup

sudo ls /etc/systemd/system/pm2*.service

export id=`id -u $USER`
sudo loginctl enable-linger $id
