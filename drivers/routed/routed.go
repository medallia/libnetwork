package routed

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	
	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/types"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/netutils"
	"github.com/vishvananda/netlink"
)

const (
	networkType = "routed"
	ifaceID     = 1
	defaultMtu	= 9000
	VethPrefix  = "vethr" 
)

type routedNetwork struct {
	id        string
	endpoints map[string]*routedEndpoint
	sync.Mutex
}

type routedEndpoint struct {
	id              string
	srcName         string
	dstName         string
	macAddress      net.HardwareAddr
	hostInterface   string
	ipv4Addresses   []netlink.Addr
}

// configuration info for the "routed" driver.
type configuration struct {
		mtu			int
}

type driver struct {
	config      *configuration
	network     *routedNetwork
	networks    map[string]*routedNetwork
	store       datastore.DataStore
	sync.Mutex
}


// Init registers a new instance of routed driver
func Init(dc driverapi.DriverCallback, config map[string]interface {}) error {
	logrus.Warnf("Init Driver %s", networkType)
	links, err := netlink.LinkList();
	if err != nil {
		logrus.Errorf("Can't get list of net devices: %s", err)
		return err
	}
	// clean up old interfaces
	for _, lnk := range links {
		if strings.HasPrefix(lnk.Attrs().Name, VethPrefix) {
			if err := netlink.LinkDel(lnk); err != nil {
				logrus.Errorf("veth couldn't be deleted: %s", lnk.Attrs().Name)
			} else {
				logrus.Infof("veth cleaned up: %s", lnk.Attrs().Name)
			}
		}
	}
	
	d := newDriver()
	d.config.mtu = defaultMtu
	c := driverapi.Capability{
		DataScope: datastore.LocalScope,
	}
	return dc.RegisterDriver(networkType, d, c)
}

// New constructs a new routed driver
func newDriver() *driver {
	return &driver{networks: map[string]*routedNetwork{}, config: &configuration{}}
}


func (d *driver) CreateNetwork(id string, option map[string]interface{}, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	logrus.Warnf("CreateNetwork: id=%s - option=%s", id, option)
	d.network = &routedNetwork{id: id, endpoints: make(map[string]*routedEndpoint)}
	return nil
}

func (d *driver) DeleteNetwork(id string) error {
	logrus.Warnf("DeleteNetwork: id=%s", id)
	d.network = nil
	return nil
}

func (d *driver) CreateEndpoint(nid, eid string, ifInfo driverapi.InterfaceInfo, epOptions map[string]interface{}) error {
	logrus.Warnf("CreatedEndpoint: %s", ifInfo)
	logrus.Debugf("nid=%s", nid)
	logrus.Debugf("eid=%s", eid)
	logrus.Debugf("epOptions= %s", epOptions)
	
	logrus.Debugf("IPV4: %s", epOptions[netlabel.IPv4Addresses])
	if epOptions[netlabel.IPv4Addresses] == nil {
		return errors.New("Empty list of IP addresses passed to the routed(local) driver")
	}
	
	// Generate host veth
	hostIfaceName, err := generateIfaceName(VethPrefix + string(eid)[:4])
	if err != nil {
		return err
	}
	logrus.Debugf("Endpoint %s Host Interface: %s", eid, hostIfaceName)

	// Generate sandbox veth
	containerIfaceName, err := generateIfaceName(VethPrefix + string(eid)[:4])
	if err != nil {
		return err
	}
	logrus.Debugf("Endpoint %s Sandbox Interface: %s", eid, containerIfaceName)

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: hostIfaceName,
			TxQLen: 0},
		PeerName: containerIfaceName}

	if err = netlink.LinkAdd(veth); err != nil {
		return err
	}

	hostIface, err := netlink.LinkByName(hostIfaceName)
	if err != nil {
		logrus.Errorf("Can't find Host Interface: %s", hostIfaceName)	
		return err
	}
	defer func() {
		if err != nil {
			logrus.Infof("Deleting Host veth %s", hostIfaceName)
			netlink.LinkDel(hostIface)
		}
	}()

	sandboxIface, err := netlink.LinkByName(containerIfaceName)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			logrus.Infof("Deleting Container veth %s", containerIfaceName)
			netlink.LinkDel(sandboxIface)
		}
	}()

	network := d.network
	network.Mutex.Lock()
	defer network.Mutex.Unlock()

	endpoint := &routedEndpoint{id: eid}
	network.endpoints[eid] = endpoint

	ipv4Addresses := epOptions[netlabel.IPv4Addresses].([]net.IPNet)
	
	if d.config.mtu != 0 {
		err = netlink.LinkSetMTU(hostIface, d.config.mtu)
		if err != nil {
			return err
		}
		err = netlink.LinkSetMTU(sandboxIface, d.config.mtu)
		if err != nil {
			return err
		}
	}

	// Down the interface before configuring mac address.
	if err := netlink.LinkSetDown(sandboxIface); err != nil {
		return fmt.Errorf("could not set link down for container interface %s: %v", containerIfaceName, err)
	}
	var imac net.HardwareAddr
	if opt, ok := epOptions[netlabel.MacAddress]; ok {
		if mac, ok := opt.(net.HardwareAddr); ok {
			logrus.Debugf("Using Mac Address: %s", mac)
			imac = mac
		}
	}
	// Set the sbox's MAC. If specified, use the one configured by user, otherwise generate one based on IP.
	mac := electMacAddress(imac, ipv4Addresses[0].IP)

	err = netlink.LinkSetHardwareAddr(sandboxIface, mac)
	if err != nil {
		return fmt.Errorf("could not set mac address %s for container interface %s: %v", mac, containerIfaceName, err)
	}

	// Up the host interface after finishing all netlink configuration
	if err := netlink.LinkSetUp(hostIface); err != nil {
		return fmt.Errorf("could not set link up for host interface %s: %v", hostIfaceName, err)
	}

	addresses := make([]netlink.Addr, len(ipv4Addresses))
	ips := make([]*net.IPNet, len(ipv4Addresses))
	for i, _ := range ipv4Addresses {
		addresses[i] = netlink.Addr{IPNet: &ipv4Addresses[i]}
		ips[i] = &ipv4Addresses[i]
	}
	// Set the primary IP Address
	if err := ifInfo.SetIPAddress(ips[0]); err != nil {
		return fmt.Errorf("could not set IP %s %v", ips[0])
	}
	// Set extra IP Addresses
	if err := ifInfo.SetExtraIPAddresses(ips[1:]); err != nil {
		return fmt.Errorf("could not set IP %s %v", ips[1:], err)
	}
	
	endpoint.srcName = containerIfaceName
	endpoint.dstName = "eth" 
	endpoint.macAddress = ifInfo.MacAddress()
	endpoint.ipv4Addresses = addresses 
	endpoint.hostInterface = hostIfaceName
	return nil
}

// ErrIfaceName error is returned when a new name could not be generated.
type ErrIfaceName struct{}

func (ein *ErrIfaceName) Error() string {
	return "failed to find name for new interface"
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	network := d.network
	endpoint := network.endpoints[eid]
	delete(network.endpoints, eid)
	// Try removal of link. Discard error: link pair might have
	// already been deleted by sandbox delete.
	link, err := netlink.LinkByName(endpoint.hostInterface)
	if err == nil {
		logrus.Debugf("Deleting host interface %s", endpoint.hostInterface)
		netlink.LinkDel(link)
	} else {
		logrus.Debugf("Can't find host interface: $s, %v ", endpoint.hostInterface, err)
	}
	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]interface{}, error) {
	return make(map[string]interface{}, 0), nil
}

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, options map[string]interface{}) error {

	network := d.network
	endpoint := network.endpoints[eid]
	
	// add route in the host to the container IP addresses.
	for _, ipv4 := range endpoint.ipv4Addresses {
		err := routeAdd(ipv4.IPNet, "", "", endpoint.hostInterface)
		if err != nil {
			logrus.Errorf("Can't Add Route to %s -> %s : %v", ipv4, endpoint.hostInterface, err)
			return err
		}
	}
	// add static default route through the veth in the sandbox
	_, dip, _ := net.ParseCIDR("0.0.0.0/0")
	if err := jinfo.AddStaticRoute(dip, types.CONNECTED, nil); err != nil {
		return fmt.Errorf("could not Add static route %v", err)
	}

	logrus.Debugf("Join Network: %s - %s", endpoint.srcName, endpoint.dstName)
	if endpoint == nil {
		return errors.New("Endpoint not found")
	}
	
	logrus.Debugf("InterfaceNameInfo: %s setNames %s -> %s", jinfo.InterfaceName(), endpoint.srcName, endpoint.dstName)
	err := jinfo.InterfaceName().SetNames(endpoint.srcName, endpoint.dstName)
	if err != nil {
		return err
	}
	return nil
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
	logrus.Infof("sandbox %s detached from endpoint %s", nid, eid)
	return nil
}

func (d *driver) Type() string {
	return networkType
}

// DiscoverNew is a notification for a new discovery event, such as a new node joining a cluster
func (d *driver) DiscoverNew(dType driverapi.DiscoveryType, data interface{}) error {
	return nil
}

// DiscoverDelete is a notification for a discovery delete event, such as a node leaving a cluster
func (d *driver) DiscoverDelete(dType driverapi.DiscoveryType, data interface{}) error {
	return nil
}


func electMacAddress(mac net.HardwareAddr, ip net.IP) net.HardwareAddr {
	if mac != nil {
		return mac
	}
	logrus.Debugf("Generating MacAddress")
	return generateMacAddr(ip)
}

func generateIfaceName(vethPrefix string) (string, error) {
	vethLen:= 12 - len(vethPrefix)
	for i := 0; i < 3; i++ {
		name, err := netutils.GenerateRandomName(vethPrefix, vethLen)
		if err != nil {
			continue
		}
		if _, err := net.InterfaceByName(name); err != nil {
			if strings.Contains(err.Error(), "no such") {
				return name, nil
			}
			return "", err
		}
	}
	return "", &ErrIfaceName{}
}

// Generate a IEEE802 compliant MAC address from the given IP address.
// The generator is guaranteed to be consistent: the same IP will always yield the same
// MAC address. This is to avoid ARP cache issues.
func generateMacAddr(ip net.IP) net.HardwareAddr {
	hw := make(net.HardwareAddr, 6)
	hw[0] = 0x02
	hw[1] = 0x42
	copy(hw[2:], ip.To4())
	return hw
}

func routeAdd(dest *net.IPNet, src string, gw string, ifaceName string) error {
	iface, _ := netlink.LinkByName(ifaceName)
	route := netlink.Route{LinkIndex: iface.Attrs().Index, Dst: dest}
	routeBroad := netlink.Route{Dst: dest}
	logrus.Infof("Adding Route in host %s", route)
	err := netlink.RouteAdd(&route)
	if err != nil {
		logrus.Debugf("Can't add route %s : %s", route, err)
		err = netlink.RouteDel(&routeBroad)
		logrus.Debugf("Deleted previous route %s :%s", routeBroad, err)
		err = netlink.RouteAdd(&route)
		logrus.Debugf("Re-Added Route %s :%s", route, err)
	}
	return err
}
