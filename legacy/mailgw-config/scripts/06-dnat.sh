sudo apt install nftables

sudo systemctl enable nftables
sudo systemctl start nftables
sudo systemctl status nftables

sudo cp -f nft/nftables.conf /etc/nftables.conf
sudo nft -f /etc/nftables.conf

# sudo nft add table nat
# sudo nft 'add chain nat prerouting { type nat hook prerouting priority -100; }'
# sudo nft 'add rule nat prerouting tcp dport 25 dnat to 127.0.0.1:2525'

sudo nft list ruleset | grep 2525
sudo nft list ruleset | grep "tcp dport"


sudo ufw allow 25/tcp
sudo ufw allow 2525/tcp
