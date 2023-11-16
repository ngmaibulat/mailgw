Haraka Mail Server provides a range of hooks that plugins can use to interact with the mail processing workflow. These hooks and their parameters are:

use:

20. **data**: Called at the DATA command.
21. **queue**: Called to queue the mail.
22. **queue_outbound**: Called to queue mail when connection.relaying is set.
23. **queue_ok**: Called when mail has been successfully queued.

24. **init_master**: Called when the main (master) process starts.
25. **init_child**: In a cluster, called when a child process starts.
26. **init_http**: Called when Haraka is started.
27. **init_wss**: Called after `init_http`.
28. **connect_init**: Used to initialize data structures, called for every connection.
29. **lookup_rdns**: Called to look up the rDNS.
30. **connect**: Called after rDNS is obtained.
31. **capabilities**: Called to get the ESMTP capabilities.
32. **unrecognized_command**: Called when an unrecognized command is received.
33. **disconnect**: Called upon disconnection.
34. **helo**: Takes hostname as a parameter.
35. **ehlo**: Takes hostname as a parameter.
36. **quit**
37. **vrfy**
38. **noop**
39. **rset**
40. **mail**: Takes 'from' and 'esmtp_params' as parameters.
41. **rcpt**: Takes 'to' and 'esmtp_params' as parameters.
42. **rcpt_ok**: Takes 'to' as a parameter.
43. **data**: Called at the DATA command.
44. **data_post**: Called at the end-of-data marker.
45. **max_data_exceeded**: Called when a message exceeds connection.max_bytes.
46. **queue**: Called to queue the mail.
47. **queue_outbound**: Called to queue mail when connection.relaying is set.
48. **queue_ok**: Called when mail has been successfully queued.
49. **reset_transaction**: Called before resetting the transaction.
50. **deny**: Called when a plugin returns DENY, DENYSOFT, or DENYDISCONNECT.
51. **get_mx**: Called by outbound to resolve the MX record.
52. **deferred**: Called when an outbound message is deferred.
53. **bounce**: Called when an outbound message bounces.
54. **delivered**: Called when outbound mail is delivered.
55. **send_email**: Called when outbound is about to be sent.
56. **pre_send_trans_email**: Called just before an email is queued to disk with a faked connection object
