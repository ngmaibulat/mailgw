
# podman generate systemd --new --name mailgw > mailgw.service

sudo cp mailgw.service /etc/systemd/system/

sudo systemctl enable mailgw.service

# journalctl -xeu mailgw.service
