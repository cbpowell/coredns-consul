// Copyright © 2022 Roberto Hidalgo <coredns-consul@un.rob.mx>
// Modified by Charles Powell, 2023
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/consul/api"
)

var watchTimeout = 10 * time.Minute

// ClientCatalog is implemented by github.com/hashicorp/consul/api.Catalog.
type ClientCatalog interface {
	Service(string, string, *api.QueryOptions) ([]*api.CatalogService, *api.QueryMeta, error)
	Services(*api.QueryOptions) (map[string][]string, *api.QueryMeta, error)
}

// KVClient is implemented by github.com/hashicorp/consul/api.Catalog.
type KVClient interface {
	Get(string, *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error)
}

type KVEntries struct {
	Target string
	ACL    []string
}

// CreateClient initializes the consul catalog client.
func CreateClient(scheme, endpoint, token string) (catalog ClientCatalog, kv KVClient, err error) {
	cfg := api.DefaultConfig()
	cfg.Address = endpoint
	if token != "" {
		cfg.Token = token
	}

	if scheme == "https" {
		cfg.Scheme = "https"
	}

	client, err := api.NewClient(cfg)

	if err != nil {
		return
	}

	catalog = client.Catalog()
	kv = client.KV()
	return
}

func (c *Catalog) FetchConfig() error {
	c.RLock()
	lastIndex := c.lastConfigIndex
	c.RUnlock()

	configPair, meta, err := c.kv.Get(c.ConfigKey, &api.QueryOptions{
		WaitTime:  watchTimeout,
		WaitIndex: lastIndex,
	})

	if err != nil {
		return err
	}

	nextIndex := meta.LastIndex

	// reset the index if it goes backwards
	// https://www.consul.io/api/features/blocking.html#implementation-details
	if nextIndex < lastIndex {
		Log.Debugf("Resetting consul kv watch index")
		nextIndex = 0
	}

	if nextIndex == lastIndex {
		// watch timed out, safe to retry
		Log.Debugf("No changes found, %d", nextIndex)
		return nil
	}

	Log.Debugf("Found config %s", configPair.Value)

	services := map[string]*Service{}
	entries := map[string]*KVEntries{}
	err = json.Unmarshal(configPair.Value, &entries)
	if err != nil {
		return err
	}

	found := []string{}
	for name, entry := range entries {
		target := entry.Target
		if target == "@service_proxy" {
			if c.ProxyService == "" {
				Log.Warningf("Ignoring service %s. Requested service proxy but none is configured", name)
				continue
			}
		}

		service := &Service{
			Name:   name,
			Target: target,
			ACL:    []*ServiceACL{},
		}

		if c.MetadataTag != "" {
			err := c.parseACL(service, entry.ACL)
			if err != nil {
				Log.Warningf("Ignoring service %s. Could not parse ACL: %s", name, err)
				continue
			}
		}

		services[name] = service
		found = append(found, name)
	}

	c.Lock()
	c.ready = true
	c.staticEntries = services
	c.lastConfigIndex = nextIndex
	c.lastUpdate = time.Now()
	c.Unlock()

	Log.Debugf("Serving records for %d kv entries: %s", len(found), strings.Join(found, ","))
	return nil
}

func (c *Catalog) parseACLString(svc *Service, acl string) error {
	aclRules := regexp.MustCompile(`;\s*`).Split(acl, -1)
	return c.parseACL(svc, aclRules)
}

func (c *Catalog) parseACL(svc *Service, rules []string) error {
	Log.Debugf("Parsing ACL for %s: %s", svc.Name, rules)
	for _, rule := range rules {
		ruleParts := strings.SplitN(rule, " ", 2)
		if len(ruleParts) != 2 {
			return fmt.Errorf("could not parse acl rule <%s>", rule)
		}
		action := ruleParts[0]
		for _, networkName := range regexp.MustCompile(`,\s*`).Split(ruleParts[1], -1) {
			if cidr, ok := c.Networks[networkName]; ok {
				svc.ACL = append(svc.ACL, &ServiceACL{
					Action:  action,
					Network: cidr,
				})
			} else {
				return fmt.Errorf("unknown network %s", networkName)
			}
		}
	}

	return nil
}

// FetchServices populates zones.
func (c *Catalog) FetchServices() error {
	c.RLock()
	lastIndex := c.lastCatalogIndex
	c.RUnlock()

	svcs, meta, err := c.client.Services(&api.QueryOptions{
		WaitTime:  watchTimeout,
		WaitIndex: lastIndex,
	})

	if err != nil {
		return err
	}

	nextIndex := meta.LastIndex
	// reset the index if it goes backwards
	// https://www.consul.io/api/features/blocking.html#implementation-details
	if nextIndex < lastIndex {
		Log.Debugf("Resetting consul catalog watch index")
		nextIndex = 0
	}

	if nextIndex == lastIndex {
		// watch timed out, safe to retry
		Log.Debugf("No changes found, %d", nextIndex)
		return nil
	}

	Log.Debugf("Found %d catalog services", len(svcs))

	found := []string{}
	currentServices := map[string]*Service{}

	for svc, serviceTags := range svcs {
		target := svc
		exposed := false
		acl_ignore := false
		aliases := []string{}
		for _, tag := range serviceTags {
			switch tag {
			case c.ACLIgnoreTag:
				Log.Debugf("ACL rules will be ignored for %s", svc)
				acl_ignore = true
			case c.ProxyTag:
				if c.ProxyTag != "" {
					Log.Debugf("%s has a proxy tag, would provide proxy as target", svc)
					target = c.ProxyService
				}
			case c.Tag:
				Log.Debugf("CoreDNS exposure is enabled for %s", svc)
				exposed = true
			default:
				// Look for alias tag definitions
				possibleAlias := strings.Split(tag, "=")
				if strings.TrimSpace(possibleAlias[0]) == c.AliasTag {
					for _, atag := range strings.Split(possibleAlias[1], ",") {
						cAtag := strings.TrimSpace(atag)
						Log.Debugf("Found alias %s for service %s", cAtag, svc)
						aliases = append(aliases, cAtag)
					}
				}
			}
		}

		// do not publish services without the tag
		if !exposed {
			Log.Debugf("%s has neither proxy tag or direct exposure tag, not exposing", svc)
			continue
		}

		hydratedServices, _, err := c.client.Service(svc, "", nil)
		if err != nil {
			// couldn't find service, ignore
			Log.Debugf("Failed to fetch service info for %s: %e", svc, err)
			continue
		}

		service := &Service{
			Name:      svc,
			Target:    target,
			ACL:       []*ServiceACL{},
			Addresses: []net.IP{},
			ApplyACL:  true,
		}

		if len(hydratedServices) > 0 {
			for _, svc := range hydratedServices {
				service.Addresses = append(service.Addresses, net.ParseIP(svc.Address))
			}
			metadata := hydratedServices[0].ServiceMeta
			if c.MetadataTag != "" {
				acl, exists := metadata[c.MetadataTag]
				if !exists {
					// No ACL for service
					if acl_ignore {
						Log.Infof("Configured to ignore ACL for service %s", svc)
						service.ApplyACL = false
					} else {
						Log.Warningf("No ACL found for service %s, will not expose", svc)
						// Continue to next service
						continue
					}
				}

				if err := c.parseACLString(service, acl); err != nil {
					Log.Warningf("Ignoring service %s: %s", service.Name, err)
				}
			}
		} else {
			Log.Warningf("No services found for %s, check the permissions for your token", svc)
		}

		// Add main service
		currentServices[svc] = service
		// Add aliases to service
		for _, alias := range aliases {
			currentServices[alias] = service
		}

		found = append(found, svc)
	}

	c.Lock()
	c.ready = true
	c.services = currentServices
	c.lastCatalogIndex = nextIndex
	c.lastUpdate = time.Now()
	c.Unlock()

	Log.Debugf("Serving records for %d catalog services: %s", len(found), strings.Join(found, ","))
	return nil
}
