In nftables, the dnat and redirect actions are used to modify the destination address of incoming packets. However, there are some differences between the two:

    Purpose: The dnat action is used to change the destination address of a packet to a different address, whereas the redirect action is used to redirect the packet to a different port on the same host.

    Destination address: The dnat action changes the destination address of the packet to a specified address, while the redirect action changes the destination address to the address of the host where the firewall is running.

    Port: The dnat action can change the destination port of the packet, while the redirect action only changes the destination port to a specified port.

    Routing: The dnat action may affect the routing of the packet, while the redirect action does not affect routing, as the packet remains on the same host.

In summary, the dnat action is used to change the destination address and port of a packet to a specified address and port, while the redirect action is used to redirect a packet to a different port on the same host.
