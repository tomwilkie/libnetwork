/*
Package libnetwork provides the basic functionality and extension points to
create network namespaces and allocate interfaces for containers to use.

    // Create a new controller instance
    controller := libnetwork.New()

    // This option is only needed for in-tree drivers. Plugins(in future) will get
    // their options through plugin infrastructure.
    option := options.Generic{}
    err := controller.NewNetworkDriver("bridge", option)
    if err != nil {
        return
    }

    netOptions := options.Generic{}
    // Create a network for containers to join.
    network, err := controller.NewNetwork("bridge", "network1", netOptions)
    if err != nil {
    	return
    }

    // For a new container: create a sandbox instance (providing a unique key).
    // For linux it is a filesystem path
    networkPath := "/var/lib/docker/.../4d23e"
    networkNamespace, err := sandbox.NewSandbox(networkPath)
    if err != nil {
	    return
    }

    // For each new container: allocate IP and interfaces. The returned network
    // settings will be used for container infos (inspect and such), as well as
    // iptables rules for port publishing.
    ep, err := network.CreateEndpoint("Endpoint1", networkNamespace.Key(), nil)
    if err != nil {
	    return
    }
*/
package libnetwork

import (
	"sync"

	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/sandbox"
	"github.com/docker/libnetwork/types"
)

// NetworkController provides the interface for controller instance which manages
// networks.
type NetworkController interface {
	// ConfigureNetworkDriver applies the passed options to the driver instance for the specified network type
	ConfigureNetworkDriver(networkType string, options interface{}) error

	// Create a new network. The options parameter carries network specific options.
	// Labels support will be added in the near future.
	NewNetwork(networkType, name string, options interface{}) (Network, error)

	// Networks returns the list of Network(s) managed by this controller.
	Networks() []Network

	// WalkNetworks uses the provided function to walk the Network(s) managed by this controller.
	WalkNetworks(walker NetworkWalker) bool

	// NetworkByName returns the Network which has the passed name, if it exists otherwise nil is returned
	NetworkByName(name string) Network

	// NetworkByID returns the Network which has the passed id, if it exists otherwise nil is returned
	NetworkByID(id string) Network
}

// A Network represents a logical connectivity zone that containers may
// join using the Link method. A Network is managed by a specific driver.
type Network interface {
	// A user chosen name for this network.
	Name() string

	// A system generated id for this network.
	ID() string

	// The type of network, which corresponds to its managing driver.
	Type() string

	// Create a new endpoint to this network symbolically identified by the
	// specified unique name. The options parameter carry driver specific options.
	// Labels support will be added in the near future.
	CreateEndpoint(name string, sboxKey string, options interface{}) (Endpoint, error)

	// Delete the network.
	Delete() error

	// Endpoints returns the list of Endpoint(s) in this network.
	Endpoints() []Endpoint

	// WalkEndpoints uses the provided function to walk the Endpoints
	WalkEndpoints(walker EndpointWalker)

	// EndpointByName returns the Endpoint which has the passed name, if it exists otherwise nil is returned
	EndpointByName(name string) Endpoint

	// EndpointByID returns the Endpoint which has the passed id, if it exists otherwise nil is returned
	EndpointByID(id string) Endpoint
}

// NetworkWalker is a client provided function which will be used to walk the Networks.
// When the function returns true, the walk will stop.
type NetworkWalker func(nw Network) bool

// Endpoint represents a logical connection between a network and a sandbox.
type Endpoint interface {
	// A system generated id for this endpoint.
	ID() string

	// Name returns the name of this endpoint.
	Name() string

	// Network returns the name of the network to which this endpoint is attached.
	Network() string

	// SandboxInfo returns the sandbox information for this endpoint.
	SandboxInfo() *sandbox.Info

	// Delete and detaches this endpoint from the network.
	Delete() error
}

// EndpointWalker is a client provided function which will be used to walk the Endpoints.
// When the function returns true, the walk will stop.
type EndpointWalker func(ep Endpoint) bool

type network struct {
	ctrlr       *controller
	name        string
	networkType string
	id          types.UUID
	driver      driverapi.Driver
	endpoints   endpointTable
	sync.Mutex
}

type endpoint struct {
	name        string
	id          types.UUID
	network     *network
	sandboxInfo *sandbox.Info
}

type networkTable map[types.UUID]*network
type endpointTable map[types.UUID]*endpoint

type controller struct {
	networks networkTable
	drivers  driverTable
	sync.Mutex
}

// New creates a new instance of network controller.
func New() NetworkController {
	return &controller{networkTable{}, enumerateDrivers(), sync.Mutex{}}
}

func (c *controller) ConfigureNetworkDriver(networkType string, options interface{}) error {
	d, ok := c.drivers[networkType]
	if !ok {
		return NetworkTypeError(networkType)
	}
	return d.Config(options)
}

// NewNetwork creates a new network of the specified network type. The options
// are network specific and modeled in a generic way.
func (c *controller) NewNetwork(networkType, name string, options interface{}) (Network, error) {
	// Check if a driver for the specified network type is available
	d, ok := c.drivers[networkType]
	if !ok {
		return nil, ErrInvalidNetworkDriver
	}

	// Check if a network already exists with the specified network name
	c.Lock()
	for _, n := range c.networks {
		if n.name == name {
			c.Unlock()
			return nil, NetworkNameError(name)
		}
	}
	c.Unlock()

	// Construct the network object
	network := &network{
		name:      name,
		id:        types.UUID(stringid.GenerateRandomID()),
		ctrlr:     c,
		driver:    d,
		endpoints: endpointTable{},
	}

	// Create the network
	if err := d.CreateNetwork(network.id, options); err != nil {
		return nil, err
	}

	// Store the network handler in controller
	c.Lock()
	c.networks[network.id] = network
	c.Unlock()

	return network, nil
}

func (c *controller) Networks() []Network {
	c.Lock()
	defer c.Unlock()

	list := make([]Network, 0, len(c.networks))
	for _, n := range c.networks {
		list = append(list, n)
	}

	return list
}

func (c *controller) WalkNetworks(walker NetworkWalker) bool {
	for _, n := range c.Networks() {
		if walker(n) {
			return true
		}
	}
	return false
}

func (c *controller) NetworkByName(name string) Network {
	var n Network

	if name != "" {
		s := func(current Network) bool {
			if current.Name() == name {
				n = current
				return true
			}
			return false
		}

		c.WalkNetworks(s)
	}

	return n
}

func (c *controller) NetworkByID(id string) Network {
	c.Lock()
	defer c.Unlock()
	if n, ok := c.networks[types.UUID(id)]; ok {
		return n
	}
	return nil
}

func (n *network) Name() string {
	return n.name
}

func (n *network) ID() string {
	return string(n.id)
}

func (n *network) Type() string {
	if n.driver == nil {
		return ""
	}

	return n.driver.Type()
}

func (n *network) Delete() error {
	var err error

	n.ctrlr.Lock()
	_, ok := n.ctrlr.networks[n.id]
	if !ok {
		n.ctrlr.Unlock()
		return &UnknownNetworkError{name: n.name, id: string(n.id)}
	}

	n.Lock()
	numEps := len(n.endpoints)
	n.Unlock()
	if numEps != 0 {
		n.ctrlr.Unlock()
		return &ActiveEndpointsError{name: n.name, id: string(n.id)}
	}

	delete(n.ctrlr.networks, n.id)
	n.ctrlr.Unlock()
	defer func() {
		if err != nil {
			n.ctrlr.Lock()
			n.ctrlr.networks[n.id] = n
			n.ctrlr.Unlock()
		}
	}()

	err = n.driver.DeleteNetwork(n.id)
	return err
}

func (n *network) CreateEndpoint(name string, sboxKey string, options interface{}) (Endpoint, error) {
	ep := &endpoint{name: name}
	ep.id = types.UUID(stringid.GenerateRandomID())
	ep.network = n

	d := n.driver
	sinfo, err := d.CreateEndpoint(n.id, ep.id, sboxKey, options)
	if err != nil {
		return nil, err
	}

	ep.sandboxInfo = sinfo
	n.Lock()
	n.endpoints[ep.id] = ep
	n.Unlock()
	return ep, nil
}

func (n *network) Endpoints() []Endpoint {
	n.Lock()
	defer n.Unlock()
	list := make([]Endpoint, 0, len(n.endpoints))
	for _, e := range n.endpoints {
		list = append(list, e)
	}

	return list
}

func (n *network) WalkEndpoints(walker EndpointWalker) {
	for _, e := range n.Endpoints() {
		if walker(e) {
			return
		}
	}
}

func (n *network) EndpointByName(name string) Endpoint {
	var e Endpoint

	if name != "" {
		s := func(current Endpoint) bool {
			if current.Name() == name {
				e = current
				return true
			}
			return false
		}

		n.WalkEndpoints(s)
	}

	return e
}

func (n *network) EndpointByID(id string) Endpoint {
	n.Lock()
	defer n.Unlock()
	if e, ok := n.endpoints[types.UUID(id)]; ok {
		return e
	}
	return nil
}

func (ep *endpoint) ID() string {
	return string(ep.id)
}

func (ep *endpoint) Name() string {
	return ep.name
}

func (ep *endpoint) Network() string {
	return ep.network.name
}

func (ep *endpoint) SandboxInfo() *sandbox.Info {
	if ep.sandboxInfo == nil {
		return nil
	}
	return ep.sandboxInfo.GetCopy()
}

func (ep *endpoint) Delete() error {
	var err error

	n := ep.network
	n.Lock()
	_, ok := n.endpoints[ep.id]
	if !ok {
		n.Unlock()
		return &UnknownEndpointError{name: ep.name, id: string(ep.id)}
	}

	delete(n.endpoints, ep.id)
	n.Unlock()
	defer func() {
		if err != nil {
			n.Lock()
			n.endpoints[ep.id] = ep
			n.Unlock()
		}
	}()

	err = n.driver.DeleteEndpoint(n.id, ep.id)
	return err
}
