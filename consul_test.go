// Copyright © 2022 Roberto Hidalgo <coredns-consul@un.rob.mx>
// SPDX-License-Identifier: Apache-2.0
package catalog

import (
	"fmt"
	"testing"

	"github.com/hashicorp/consul/api"
)

type testServiceData struct {
	Tags    []string
	Meta    map[string]string
	Address string
}

type TestCatalogClient struct {
	services  map[string][]*testServiceData
	lastIndex uint64
}

func NewTestCatalogClient() ClientCatalog {
	return &TestCatalogClient{
		lastIndex: 4,
		services: map[string][]*testServiceData{
			"nomad": {
				{
					Address: "192.168.100.1",
					Tags: []string{
						"coredns.enabled",
						"traefik.enable=true",
					},
					Meta: map[string]string{
						"coredns-acl": "allow private",
					},
				},
			},
			"nomad-client": {
				{
					Address: "192.168.100.1",
					Tags:    []string{},
					Meta:    map[string]string{},
				},
			},
			"traefik": {
				{
					Address: "192.168.100.2",
					Tags: []string{
						"coredns.enabled",
						"traefik.enable=true",
					},
					Meta: map[string]string{
						"coredns-acl": "allow private, guest; deny public",
					},
				},
			},
			"git": {
				{
					Address: "192.168.100.3",
					Tags:    []string{"coredns.enabled"},
					Meta: map[string]string{
						"coredns-acl": "deny guest; allow public",
					},
				},
				{
					Address: "192.168.100.4",
					Tags:    []string{"coredns.enabled"},
					Meta: map[string]string{
						"coredns-acl": "deny guest; allow public",
					},
				},
			},
		},
	}
}

func (c *TestCatalogClient) DeleteService(name string) {
	if _, ok := c.services[name]; !ok {
		Log.Infof("deleting unknong service")
	}
	delete(c.services, name)
}

func (c *TestCatalogClient) Service(name string, tag string, opts *api.QueryOptions) ([]*api.CatalogService, *api.QueryMeta, error) {
	sd, ok := c.services[name]
	if !ok {
		return []*api.CatalogService{}, nil, fmt.Errorf("Not found")
	}

	services := []*api.CatalogService{}
	for _, nodeService := range sd {
		services = append(services, &api.CatalogService{
			ID:          "42",
			ServiceName: name,
			Node:        fmt.Sprintf("node-%s", nodeService.Address),
			Address:     nodeService.Address,
			ServiceMeta: nodeService.Meta,
			ServiceTags: nodeService.Tags,
		})
	}
	return services, &api.QueryMeta{}, nil
}

func (c *TestCatalogClient) Services(*api.QueryOptions) (map[string][]string, *api.QueryMeta, error) {
	services := map[string][]string{}
	for name, svc := range c.services {
		services[name] = svc[0].Tags
	}

	c.lastIndex = uint64(len(services))
	return services, &api.QueryMeta{LastIndex: c.lastIndex}, nil
}

type TestKVClient struct{}

func NewTestKVClient() KVClient {
	return &TestKVClient{}
}

func (kv *TestKVClient) Get(path string, opts *api.QueryOptions) (*api.KVPair, *api.QueryMeta, error) {
	data := []byte(`{"consul": {"target": "traefik", "acl": ["allow private"]}}`)
	return &api.KVPair{
		Value: data,
	}, &api.QueryMeta{LastIndex: 1}, nil
}

func TestFetchConfig(t *testing.T) {
	c, _ := NewTestCatalog(false)

	if err := c.FetchConfig(); err != nil {
		t.Fatalf("Failed fetching config, %s", err)
	}

	svc := c.ServiceFor("consul")
	if svc == nil {
		t.Fatalf("Service consul not found")
	}

	if svc.Target != "traefik" {
		t.Fatalf("Unexpected target: %v", svc.Target)
	}
}

func TestFetchServices(t *testing.T) {
	c, client := NewTestCatalog(true)

	services := c.Services()
	if len(services) != 3 {
		t.Fatalf("Unexpected number of services: %d", len(services))
	}

	svcTests := map[string]string{
		"nomad":   "traefik",
		"traefik": "traefik",
		"git":     "git",
	}
	for svc, expected := range svcTests {
		target, exists := services[svc]
		if !exists {
			t.Fatalf("Expected service %s not found", svc)
		}

		if target.Target != expected {
			t.Fatalf("Unexpected target: %v", target)
		}
	}

	lastUpdate := c.LastUpdated()
	err := c.FetchServices()
	if err != nil {
		t.Fatalf("Fetch services: %v", err)
	}

	if lastUpdate != c.LastUpdated() {
		t.Fatalf("Services changed after timeout")
	}

	err = c.FetchServices()
	if err != nil {
		t.Fatalf("Fetch services: %v", err)
	}

	testclient := client.(*TestCatalogClient)
	testclient.DeleteService("git")
	if err := c.FetchServices(); err != nil {
		t.Fatalf("could not fetch services: %s", err)
	}

	if lastUpdate == c.LastUpdated() {
		t.Fatalf("Services did not change after update")
	}

	if newCount := len(c.Services()); newCount != 2 {
		t.Fatalf("Unexpected number of services after update: %d", newCount)
	}
}
