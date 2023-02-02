### Install

sudo apt install nala
sudo nala install podman
sudo nala install cockpit
sudo nala install cockpit-podman

### Pull

podman pull docker.io/ngmaibulat/mailgw

### Use

export dir=`pwd`
podman run --name mailgw \
 --mount type=bind,source=$dir/config,target=/opt/mailgw/config \
 -d ngmaibulat/mailgw
