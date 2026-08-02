### Install Docker
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates curl gnupg2 software-properties-common

sudo mv /usr/share/keyrings/docker-archive-keyring.gpg .
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg

echo yes | sudo add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable"

sudo apt-get update
sudo apt-get install -y docker-ce
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
