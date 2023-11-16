Haraka Mail Server provides a range of hooks that plugins can use to interact with the mail processing workflow. These hooks and their parameters are:

1. **init_master**: Called when the main (master) process starts.
2. **init_child**: In a cluster, called when a child process starts.
3. **init_http**: Called when Haraka is started.
4. **init_wss**: Called after `init_http`.
5. **connect_init**: Used to initialize data structures, called for every connection.
6. **lookup_rdns**: Called to look up the rDNS.
7. **connect**: Called after rDNS is obtained.
8. **capabilities**: Called to get the ESMTP capabilities.
9. **unrecognized_command**: Called when an unrecognized command is received.
10. **disconnect**: Called upon disconnection.
11. **helo**: Takes hostname as a parameter.
12. **ehlo**: Takes hostname as a parameter.
13. **quit**
14. **vrfy**
15. **noop**
16. **rset**
17. **mail**: Takes 'from' and 'esmtp_params' as parameters.
18. **rcpt**: Takes 'to' and 'esmtp_params' as parameters.
19. **rcpt_ok**: Takes 'to' as a parameter.
20. **data**: Called at the DATA command.
21. **data_post**: Called at the end-of-data marker.
22. **max_data_exceeded**: Called when a message exceeds connection.max_bytes.
23. **queue**: Called to queue the mail.
24. **queue_outbound**: Called to queue mail when connection.relaying is set.
25. **queue_ok**: Called when mail has been successfully queued.
26. **reset_transaction**: Called before resetting the transaction.
27. **deny**: Called when a plugin returns DENY, DENYSOFT, or DENYDISCONNECT.
28. **get_mx**: Called by outbound to resolve the MX record.
29. **deferred**: Called when an outbound message is deferred.
30. **bounce**: Called when an outbound message bounces.
31. **delivered**: Called when outbound mail is delivered.
32. **send_email**: Called when outbound is about to be sent.
33. **pre_send_trans_email**: Called just before an email is queued to disk with a faked connection object
