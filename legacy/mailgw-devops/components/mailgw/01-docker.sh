### Install Docker

sudo apt-get install docker.io
sudo docker --version

sudo usermod -aG docker $USER
newgrp docker

sudo systemctl start docker
sudo systemctl enable docker

# Note for the user
echo "Please log out and back in to apply Docker group changes, or run 'newgrp docker' in your terminal."

### Mainly relevant to PM2 and Podman. But leave it here for now.
### User systemd services can run after user logout
export id=`id -u $USER`
sudo loginctl enable-linger $id
