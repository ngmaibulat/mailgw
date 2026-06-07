- cifs-utils
- docker
- vim
- tcpdump

sudo mount -t cifs -o username=yourusername,vers=3.0 //server/sharename /mnt/smbshare
sudo mount -t cifs -o vers=2.0,credentials=/path/to/credentialsfile //server/share /mnt/share

//server/sharename /mnt/smbshare cifs credentials=/path/to/credentialsfile,vers=3.0 0 0

username=yourusername
password=yourpassword
