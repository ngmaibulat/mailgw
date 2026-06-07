curl -X POST -H "Content-Type: application/json" -d @connection/conn.json http://$IP:3000/api/connection

curl -X POST -H "Content-Type: application/json" -d @delivery/delivery.json http://$IP:3000/api/delivery
