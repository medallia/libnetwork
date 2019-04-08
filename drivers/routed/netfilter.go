package routed

import (
	"fmt"
	"net"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libnetwork/netlabel"
)

const (
	containersChainName      = "CONTAINERS"
	containerRejectChainName = "CONTAINER-REJECT"
	vethChainPrefix          = "CONTAINER-"
)

// IPRange range of ip addresses used to filter
type IPRange struct {
	from net.IP
	to   net.IP
}

// ParseIPRange parses the given string into an IPRange
func ParseIPRange(ipRange string) *IPRange {
	ipStrs := strings.Split(ipRange, "-")
	if len(ipStrs) == 2 {
		var ips [2]net.IP
		for idx, ipStr := range ipStrs {
			ips[idx] = net.ParseIP(strings.TrimSpace(ipStr))
		}
		if ips[0] != nil && ips[1] != nil {
			return &IPRange{from: ips[0], to: ips[1]}
		}
	}
	return nil
}

func (r *IPRange) String() string {
	return r.from.String() + "-" + r.to.String()
}

type netFilterConfig struct {
	allowedNets   []*net.IPNet
	allowedRanges []*IPRange
}

type netFilter struct {
	ifaceName string
	config    *netFilterConfig
}

// ParseIPOrNet parses the given string into an IPNet
func ParseIPOrNet(ipStr string) *net.IPNet {
	if !strings.Contains(ipStr, "/") {
		ipStr += "/32"
	}

	if _, ipNet, err := net.ParseCIDR(ipStr); err == nil {
		return ipNet
	}
	return nil
}

func NetFilterConfigParse(ingressAllowedString string) (*netFilterConfig, error) {
	if ingressAllowedString != "" {
		config := new(netFilterConfig)
		for _, filterElement := range strings.Split(ingressAllowedString, ",") {
			filterElement = strings.TrimSpace(filterElement)
			ipNet := ParseIPOrNet(filterElement)
			if ipNet == nil {
				if ipRange := ParseIPRange(filterElement); ipRange != nil {
					config.allowedRanges = append(config.allowedRanges, ipRange)
				} else {
					return nil, fmt.Errorf("NetFilter: Could not parse IP, CIDR or IPRange %s", filterElement)
				}
			} else {
				config.allowedNets = append(config.allowedNets, ipNet)
			}
		}
		return config, nil
	}
	return nil, nil
}

func newNetFilter(ifaceName string, epOptions map[string]interface{}) *netFilter {
	logrus.Debugf("New NetFilter for iface %s and options %s", ifaceName, epOptions)

	ingressFiltering := epOptions[netlabel.IngressAllowed].(*netFilterConfig)
	if ingressFiltering == nil {
		logrus.Info("NetFilter: No network ingress filtering specified")
	}

	return &netFilter{ifaceName, ingressFiltering}
}

func chainExists(chainName string) bool {
	return iptables.ExistChain(chainName, iptables.Filter)
}

func (n *netFilter) applyFiltering() error {
	if n.config == nil {
		return nil // Net Filtering disabled
	}

	vethChainName := vethChainPrefix + n.ifaceName

	logrus.Debugf("NetFilter. Allowing ingress: %s %s for %s", n.config.allowedNets, n.config.allowedRanges, n.ifaceName)

	// Verify expected chains "CONTAINERS" and "CONTAINER-REJECT" exist
	for _, chainName := range []string{containersChainName, containerRejectChainName} {
		if !chainExists(chainName) {
			return fmt.Errorf("Expected iptables chain not found: %s", chainName)
		}
	}

	rules := new(iptablesRules)
	rules.addRule("-N", vethChainName) // create veth specific chain

	// Allow specified nets and ranges only
	for _, ipNet := range n.config.allowedNets {
		rules.addRule("-A", vethChainName, "-s", ipNet.String(), "-j", "ACCEPT")
	}
	for _, ipRange := range n.config.allowedRanges {
		rules.addRule("-A", vethChainName, "-m", "iprange", "--src-range", ipRange.String(), "-j", "ACCEPT")
	}

	rules.addRule("-A", vethChainName, "-j", "CONTAINER-REJECT")

	// Add JUMP in CONTAINERS, send all traffic going to the veth interface
	rules.addRule("-I", containersChainName, "1", "-o", n.ifaceName, "-j", vethChainName)

	if err := rules.apply(); err != nil {
		return err
	}

	logrus.Info("NetFilter: Successfully applied ingress filtering")
	return nil
}

func (n *netFilter) removeFiltering() error {
	if n.config == nil {
		return nil
	}

	logrus.Debugf("NetFilter. Removing rules for %s", n.ifaceName)

	vethChainName := vethChainPrefix + n.ifaceName

	rules := new(iptablesRules)
	rules.addRule("-D", containersChainName, "-o", n.ifaceName, "-j", vethChainName)
	rules.addRule("-F", vethChainName)
	rules.addRule("-X", vethChainName)
	return rules.apply()
}

type iptablesRules struct {
	rules [][]string
}

func (ipRules *iptablesRules) addRule(args ...string) {
	ipRules.rules = append(ipRules.rules, args)
}

func (ipRules *iptablesRules) apply() error {
	for _, rule := range ipRules.rules {
		if err := applyIPTablesRule(rule...); err != nil {
			return err
		}
	}
	return nil
}

func applyIPTablesRule(args ...string) error {
	logrus.Debugf("NetFilter. IpTables call %s", args)
	if output, err := iptables.Raw(args...); err != nil {
		return fmt.Errorf("NetFilter. IP tables apply rule failed %s %s %v", args, output, err)
	}
	return nil
}
