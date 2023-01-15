- test via git clone
- npm package: scaffolder
- docker
- kuber

### Important

- logDelivery plugin

### Upgrade plan

- remove traffic via DNS
- stop pm2 job
- update nodejs
- update pm2
- create folder for blue deployment
- use scaffolder for the deployment
- copy the config json files
- run the blue deployment via pm2
- remove pm2 job for the green deployment
- save pm2 jobs
- apply OS updates
- restart
- test PM2 job is running
- test smtp flow
- restore DNS records
- provide instructions/scripts for the rollback
- plan upgrade for 2nd server node
