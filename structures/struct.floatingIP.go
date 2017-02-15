package structures

type FloatingIP struct {
	// Unique identifier for the floating IP instance.
	ID  string `json:"id" mapstructure:"id"`

	// UUID of the external network where the floating IP is to be created.
	FloatingNetworkID string `json:"floating_network_id" mapstructure:"floating_network_id"`

	// Address of the floating IP on the external network.
	FloatingIP string `json:"floating_ip_address" mapstructure:"floating_ip_address"`

	// UUID of the port on an internal network that is associated with the floating IP.
	PortID string `json:"port_id" mapstructure:"port_id"`

	// The specific IP address of the internal port which should be associated
	// with the floating IP.
	FixedIP string `json:"fixed_ip_address" mapstructure:"fixed_ip_address"`

	// Owner of the floating IP. Only admin users can specify a tenant identifier
	// other than its own.
	TenantID string `json:"tenant_id" mapstructure:"tenant_id"`

	// The condition of the API resource.
	Status string `json:"status" mapstructure:"status"`
}