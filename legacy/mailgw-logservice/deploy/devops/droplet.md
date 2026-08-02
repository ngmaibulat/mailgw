To pass the IP address of a DigitalOcean Droplet created by one Ansible role to the inventory for use in other roles or playbooks, you can follow these steps:

### 1. Capture the Droplet's IP Address

When you create a Droplet using Ansible (e.g., using the `digital_ocean_droplet` module), the module typically returns information about the Droplet, including its IP address. You can register this information in a variable.

Example:

```yaml
- name: Create a DigitalOcean Droplet
  digital_ocean_droplet:
    # ... your Droplet configuration ...
  register: droplet_info
```

The IP address can be found within `droplet_info` under the `ip_address` attribute (or similar, depending on the Ansible version and module).

### 2. Add the IP Address to the Inventory

You can dynamically add the new Droplet's IP address to your Ansible inventory using the `add_host` module. This makes the new host available to subsequent plays in the same playbook.

Example:

```yaml
- name: Add new Droplet to inventory
  add_host:
    name: "{{ droplet_info.data.droplet.networks.v4[0].ip_address }}"
    groups: "group_name"
    # You can also set variables for the new host here
```

In this example, replace `group_name` with the name of the group you want to add this host to in the inventory. You can also add any variables you might need for the new host.

### 3. Use the IP Address in Subsequent Roles/Playbooks

Now that the new Droplet is added to your inventory, you can reference it in subsequent roles or playbooks just like any other host in your inventory.

- If you're using roles, ensure that the role targets the appropriate group or host.
- If you're running separate playbooks, you might want to export the IP address to an external source (like a file or environment variable) or use a shared inventory that maintains state between playbook runs.

### Example Playbook Structure

```yaml
- hosts: localhost
  tasks:
    - name: Create a DigitalOcean Droplet
      # ... Droplet creation task ...
      register: droplet_info

    - name: Add new Droplet to inventory
      # ... Add host task ...

- hosts: group_name
  roles:
    - some_other_role
```

### Note on Persistent Inventory

For more permanent solutions or when dealing with complex deployments, consider using a dynamic inventory script/plugin for DigitalOcean. This way, your Ansible inventory always reflects the current state of your DigitalOcean environment. Ansible includes a DigitalOcean dynamic inventory script that you can use for this purpose.
