package consulrangeplugin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/coredhcp/coredhcp/handler"
	"github.com/coredhcp/coredhcp/logger"
	"github.com/coredhcp/coredhcp/plugins"
	"github.com/coredhcp/coredhcp/plugins/allocators"
	"github.com/coredhcp/coredhcp/plugins/allocators/bitmap"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/hashicorp/consul/api"
)

var log = logger.GetLogger("plugins/consulrange")

// Plugin wraps plugin registration information
var Plugin = plugins.Plugin{
	Name:   "consulrange",
	Setup4: setupConsulRange,
}

// Record represents a DHCP lease record.
type Record struct {
	IP       net.IP `json:"ip"`
	Expires  int    `json:"expires"`  // for example, a Unix timestamp
	Hostname string `json:"hostname"` // the client hostname
}

// PluginState is the data held by an instance of the consul plugin
type PluginState struct {
	// Rough lock for the whole plugin, we'll get better performance once we use leasestorage
	sync.Mutex
	// Recordsv4 holds a MAC -> IP address and lease time mapping
	Recordsv4      map[string]*Record
	LeaseTime      time.Duration
	allocator      allocators.Allocator
	consulURL      string
	consulKVPrefix string
	consulClient   *api.Client
}

// Handler4 handles DHCPv4 packets for the range plugin
func (p *PluginState) Handler4(req, resp *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, bool) {
	p.Lock()
	defer p.Unlock()
	record, ok := p.Recordsv4[req.ClientHWAddr.String()]
	hostname := req.HostName()
	if !ok {
		// Allocating new address since there isn't one allocated
		log.Printf("MAC address %s is new, leasing new IPv4 address", req.ClientHWAddr.String())
		ip, err := p.allocator.Allocate(net.IPNet{})
		if err != nil {
			log.Errorf("Could not allocate IP for MAC %s: %v", req.ClientHWAddr.String(), err)
			return nil, true
		}
		rec := Record{
			IP:       ip.IP.To4(),
			Expires:  int(time.Now().Add(p.LeaseTime).Unix()),
			Hostname: hostname,
		}
		err = p.saveIPAddress(req.ClientHWAddr, &rec)
		if err != nil {
			log.Errorf("SaveIPAddress for MAC %s failed: %v", req.ClientHWAddr.String(), err)
		}
		p.Recordsv4[req.ClientHWAddr.String()] = &rec
		record = &rec
	} else {
		// Ensure we extend the existing lease at least past when the one we're giving expires
		expiry := time.Unix(int64(record.Expires), 0)
		if expiry.Before(time.Now().Add(p.LeaseTime)) {
			record.Expires = int(time.Now().Add(p.LeaseTime).Round(time.Second).Unix())
			record.Hostname = hostname
			err := p.saveIPAddress(req.ClientHWAddr, record)
			if err != nil {
				log.Errorf("Could not persist lease for MAC %s: %v", req.ClientHWAddr.String(), err)
			}
		}
	}
	resp.YourIPAddr = record.IP
	resp.Options.Update(dhcpv4.OptIPAddressLeaseTime(p.LeaseTime.Round(time.Second)))
	log.Printf("found IP address %s for MAC %s", record.IP, req.ClientHWAddr.String())
	return resp, false
}

func setupConsulRange(args ...string) (handler.Handler4, error) {
	var (
		err error
		p   PluginState
	)

	if len(args) < 5 {
		return nil, fmt.Errorf("invalid number of arguments, want: 4 (Consul base URL, KV prefix, start IP, end IP, lease time), got: %d", len(args))
	}
	consulURL := args[0]
	if consulURL == "" {
		return nil, errors.New("Consul URL cannot be empty")
	}

	consulKVPrefix := args[1]
	if consulKVPrefix == "" {
		return nil, errors.New("Consul KV prefix cannot be empty")
	}

	ipRangeStart := net.ParseIP(args[2])
	if ipRangeStart.To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 address: %v", args[2])
	}
	ipRangeEnd := net.ParseIP(args[3])
	if ipRangeEnd.To4() == nil {
		return nil, fmt.Errorf("invalid IPv4 address: %v", args[3])
	}
	if binary.BigEndian.Uint32(ipRangeStart.To4()) >= binary.BigEndian.Uint32(ipRangeEnd.To4()) {
		return nil, errors.New("start of IP range has to be lower than the end of an IP range")
	}

	p.allocator, err = bitmap.NewIPv4Allocator(ipRangeStart, ipRangeEnd)
	if err != nil {
		return nil, fmt.Errorf("could not create an allocator: %w", err)
	}

	p.LeaseTime, err = time.ParseDuration(args[4])
	if err != nil {
		return nil, fmt.Errorf("invalid lease duration: %v", args[4])
	}

	p.consulURL = consulURL
	p.consulKVPrefix = consulKVPrefix

	// Create a new Consul API client.
	config := api.DefaultConfig()
	config.Address = consulURL
	client, err := api.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create consul client: %w", err)
	}

	p.consulClient = client

	p.Recordsv4, err = loadRecords(p.consulClient, p.consulKVPrefix)
	if err != nil {
		return nil, fmt.Errorf("could not load records from file: %v", err)
	}

	log.Printf("Loaded %d DHCPv4 leases from %s", len(p.Recordsv4), consulURL)

	for _, v := range p.Recordsv4 {
		ip, err := p.allocator.Allocate(net.IPNet{IP: v.IP})
		if err != nil {
			return nil, fmt.Errorf("failed to re-allocate leased ip %v: %v", v.IP.String(), err)
		}
		if ip.IP.String() != v.IP.String() {
			return nil, fmt.Errorf("allocator did not re-allocate requested leased ip %v: %v", v.IP.String(), ip.String())
		}
	}

	return p.Handler4, nil
}
