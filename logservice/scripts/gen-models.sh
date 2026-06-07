npx sequelize-cli model:generate \
    --name Connection \
    --attributes \
    dt:date,uuid:string,encoding:string,hello_name:string


npx sequelize-cli model:generate \
    --name Transaction \
    --attributes \
    dt:date,uuid:string,encoding:string,sender:string,rcpt_list:string,rcpt_count_accept:integer,rcpt_count_tempfail:integer,rcpt_count_reject:integer


npx sequelize-cli model:generate \
    --name Delivery \
    --attributes \
    dt:date,uuid:string,sender:string,rcpt_list:string,rcpt_domain:string,rcpt_accepted:integer


npx sequelize-cli model:generate \
    --name Header \
    --attributes \
    mail_id:integer,name:string,value:string


npx sequelize-cli model:generate \
    --name MailAddr \
    --attributes \
    mail_id:integer,name:string,email:string


npx sequelize-cli model:generate \
    --name linkAddrTransaction \
    --attributes \
    MailAddrId:integer,TransactionId:integer


npx sequelize-cli model:generate \
    --name BlockMD5 \
    --attributes \
    md5:string,comment:string

npx sequelize-cli model:generate \
    --name HashLookup \
    --attributes \
    txn_uuid:string,md5:string,contentType:string,filename:string,size:integer,action:string

npx sequelize-cli model:generate \
    --name RelayGroup \
    --attributes \
    name:string,description:string

npx sequelize-cli model:generate \
    --name Relay \
    --attributes \
    group_id:integer,name:string,host:string,port:integer,auth_user:string,auth_pass:string,priority:integer


npx sequelize-cli model:generate \
    --name Config \
    --attributes \
    name:string,value:string


npx sequelize-cli model:generate \
    --name Log \
    --attributes \
    url:string,path:string,query:string,src_ip:string,src_port:integer,referer:string,origin:string,method:string,user:string,userAgent:string

npx sequelize-cli model:generate \
    --name User \
    --attributes \
    email:string,hash:string

npx sequelize-cli model:generate \
    --name Exception \
    --attributes \
    product:string,component:string,info:text
