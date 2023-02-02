echo "stop pm2 task for MailGW before this script"

read -p "Press [Enter] to continue"

cd config

mv *.json ../..

ln -s ../../logging.json logging.json
ln -s ../../relays.json relays.json
ln -s ../../routing.json routing.json
