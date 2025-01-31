// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package snmp

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"

	"akvorado/common/daemon"
	"akvorado/common/helpers"
	"akvorado/common/reporter"
)

func expectSNMPLookup(t *testing.T, c *Component, exporter string, ifIndex uint, expected answer) {
	t.Helper()
	ip := netip.AddrFrom16(netip.MustParseAddr(exporter).As16())
	gotExporterName, gotInterface, ok := c.Lookup(time.Now(), ip, ifIndex)
	got := answer{gotExporterName, gotInterface, !ok}
	if diff := helpers.Diff(got, expected); diff != "" {
		t.Fatalf("Lookup() (-got, +want):\n%s", diff)
	}
}

func TestLookup(t *testing.T) {
	r := reporter.NewMock(t)
	c := NewMock(t, r, DefaultConfiguration(), Dependencies{Daemon: daemon.NewMock(t)})

	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{NOk: true})
	expectSNMPLookup(t, c, "127.0.0.1", 999, answer{NOk: true})
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
		ExporterName: "127_0_0_1",
		Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
	})
	expectSNMPLookup(t, c, "127.0.0.1", 999, answer{
		ExporterName: "127_0_0_1",
	})
}

func TestSNMPCommunities(t *testing.T) {
	r := reporter.NewMock(t)
	configuration := DefaultConfiguration()
	configuration.Communities, _ = helpers.NewSubnetMap(map[string]string{
		"::/0":                 "notpublic",
		"::ffff:127.0.0.1/128": "public",
		"::ffff:127.0.0.2/128": "private",
	})
	c := NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t)})

	// Use "public" as a community. Should work.
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{NOk: true})
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
		ExporterName: "127_0_0_1",
		Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
	})

	// Use "private", should not work
	expectSNMPLookup(t, c, "127.0.0.2", 765, answer{NOk: true})
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.2", 765, answer{NOk: true})

	// Use default community, should not work
	expectSNMPLookup(t, c, "127.0.0.3", 765, answer{NOk: true})
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.3", 765, answer{NOk: true})
}

func TestComponentSaveLoad(t *testing.T) {
	configuration := DefaultConfiguration()
	configuration.CachePersistFile = filepath.Join(t.TempDir(), "cache")

	t.Run("save", func(t *testing.T) {
		r := reporter.NewMock(t)
		c := NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t)})

		expectSNMPLookup(t, c, "127.0.0.1", 765, answer{NOk: true})
		time.Sleep(30 * time.Millisecond)
		expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
			ExporterName: "127_0_0_1",
			Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
		})
	})

	t.Run("load", func(t *testing.T) {
		r := reporter.NewMock(t)
		c := NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t)})
		expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
			ExporterName: "127_0_0_1",
			Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
		})
	})
}

func TestAutoRefresh(t *testing.T) {
	r := reporter.NewMock(t)
	configuration := DefaultConfiguration()
	mockClock := clock.NewMock()
	c := NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t), Clock: mockClock})

	// Fetch a value
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{NOk: true})
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
		ExporterName: "127_0_0_1",
		Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
	})

	// Keep it in the cache!
	mockClock.Add(25 * time.Minute)
	c.Lookup(mockClock.Now(), netip.MustParseAddr("::ffff:127.0.0.1"), 765)
	mockClock.Add(25 * time.Minute)
	c.Lookup(mockClock.Now(), netip.MustParseAddr("::ffff:127.0.0.1"), 765)

	// Go forward, we expect the entry to have been refreshed and be still present
	mockClock.Add(11 * time.Minute)
	time.Sleep(30 * time.Millisecond)
	mockClock.Add(2 * time.Minute)
	time.Sleep(30 * time.Millisecond)
	expectSNMPLookup(t, c, "127.0.0.1", 765, answer{
		ExporterName: "127_0_0_1",
		Interface:    Interface{Name: "Gi0/0/765", Description: "Interface 765", Speed: 1000},
	})

	gotMetrics := r.GetMetrics("akvorado_inlet_snmp_cache_")
	expectedMetrics := map[string]string{
		`expired`:      "0",
		`hit`:          "4",
		`miss`:         "1",
		`size`:         "1",
		`refresh_runs`: "31", // 63/2
		`refresh`:      "1",
	}
	if diff := helpers.Diff(gotMetrics, expectedMetrics); diff != "" {
		t.Fatalf("Metrics (-got, +want):\n%s", diff)
	}
}

func TestConfigCheck(t *testing.T) {
	t.Run("refresh", func(t *testing.T) {
		configuration := DefaultConfiguration()
		configuration.CacheDuration = 10 * time.Minute
		configuration.CacheRefresh = 5 * time.Minute
		configuration.CacheCheckInterval = time.Minute
		if _, err := New(reporter.NewMock(t), configuration, Dependencies{Daemon: daemon.NewMock(t)}); err == nil {
			t.Fatal("New() should trigger an error")
		}
	})
	t.Run("interval", func(t *testing.T) {
		configuration := DefaultConfiguration()
		configuration.CacheDuration = 10 * time.Minute
		configuration.CacheRefresh = 15 * time.Minute
		configuration.CacheCheckInterval = 12 * time.Minute
		if _, err := New(reporter.NewMock(t), configuration, Dependencies{Daemon: daemon.NewMock(t)}); err == nil {
			t.Fatal("New() should trigger an error")
		}
	})
	t.Run("refresh disabled", func(t *testing.T) {
		configuration := DefaultConfiguration()
		configuration.CacheDuration = 10 * time.Minute
		configuration.CacheRefresh = 0
		configuration.CacheCheckInterval = 2 * time.Minute
		if _, err := New(reporter.NewMock(t), configuration, Dependencies{Daemon: daemon.NewMock(t)}); err != nil {
			t.Fatalf("New() error:\n%+v", err)
		}
	})
}

func TestStartStopWithMultipleWorkers(t *testing.T) {
	r := reporter.NewMock(t)
	configuration := DefaultConfiguration()
	configuration.Workers = 5
	NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t)})
}

type logCoalescePoller struct {
	received []lookupRequest
}

func (fcp *logCoalescePoller) Poll(_ context.Context, exporterIP, _ netip.Addr, _ uint16, ifIndexes []uint) error {
	fcp.received = append(fcp.received, lookupRequest{exporterIP, ifIndexes})
	return nil
}

func TestCoalescing(t *testing.T) {
	lcp := &logCoalescePoller{
		received: []lookupRequest{},
	}
	r := reporter.NewMock(t)
	t.Run("run", func(t *testing.T) {
		c := NewMock(t, r, DefaultConfiguration(), Dependencies{Daemon: daemon.NewMock(t)})
		c.poller = lcp

		// Block dispatcher
		blocker := make(chan bool)
		c.dispatcherBChannel <- blocker

		defer func() {
			// Unblock
			time.Sleep(20 * time.Millisecond)
			close(blocker)
			time.Sleep(20 * time.Millisecond)
		}()

		// Queue requests
		expectSNMPLookup(t, c, "127.0.0.1", 766, answer{NOk: true})
		expectSNMPLookup(t, c, "127.0.0.1", 767, answer{NOk: true})
		expectSNMPLookup(t, c, "127.0.0.1", 768, answer{NOk: true})
		expectSNMPLookup(t, c, "127.0.0.1", 769, answer{NOk: true})
	})

	gotMetrics := r.GetMetrics("akvorado_inlet_snmp_poller_", "coalesced_count")
	expectedMetrics := map[string]string{
		`coalesced_count`: "4",
	}
	if diff := helpers.Diff(gotMetrics, expectedMetrics); diff != "" {
		t.Errorf("Metrics (-got, +want):\n%s", diff)
	}

	expectedAccepted := []lookupRequest{
		{netip.MustParseAddr("::ffff:127.0.0.1"), []uint{766, 767, 768, 769}},
	}
	if diff := helpers.Diff(lcp.received, expectedAccepted); diff != "" {
		t.Errorf("Accepted requests (-got, +want):\n%s", diff)
	}
}

type errorPoller struct{}

func (fcp *errorPoller) Poll(_ context.Context, _, _ netip.Addr, _ uint16, _ []uint) error {
	return errors.New("noooo")
}

func TestPollerBreaker(t *testing.T) {
	cases := []struct {
		Name          string
		Poller        poller
		ExpectedCount string
	}{
		{"always successful poller", nil, "0"},
		{"never successful poller", &errorPoller{}, "10"},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			r := reporter.NewMock(t)
			configuration := DefaultConfiguration()
			configuration.PollerCoalesce = 0
			c := NewMock(t, r, configuration, Dependencies{Daemon: daemon.NewMock(t)})
			if tc.Poller != nil {
				c.poller = tc.Poller
			}
			c.metrics.pollerBreakerOpenCount.WithLabelValues("127.0.0.1").Add(0)

			for i := 0; i < 30; i++ {
				c.Lookup(c.d.Clock.Now(), netip.MustParseAddr("::ffff:127.0.0.1"), 765)
			}
			for i := 0; i < 5; i++ {
				c.Lookup(c.d.Clock.Now(), netip.MustParseAddr("::ffff:127.0.0.2"), 765)
			}
			time.Sleep(50 * time.Millisecond)

			gotMetrics := r.GetMetrics("akvorado_inlet_snmp_poller_", "breaker_open_count", "coalesced_count")
			expectedMetrics := map[string]string{
				`coalesced_count`:                          "0",
				`breaker_open_count{exporter="127.0.0.1"}`: tc.ExpectedCount,
			}
			if diff := helpers.Diff(gotMetrics, expectedMetrics); diff != "" {
				t.Errorf("Metrics (-got, +want):\n%s", diff)
			}
		})
	}
}

type agentLogPoller struct {
	lastExporter string
	lastAgent    string
	mu           sync.Mutex
}

func (alp *agentLogPoller) Poll(_ context.Context, exporterIP, agentIP netip.Addr, _ uint16, _ []uint) error {
	alp.mu.Lock()
	defer alp.mu.Unlock()
	alp.lastExporter = exporterIP.Unmap().String()
	alp.lastAgent = agentIP.Unmap().String()
	return nil
}

func TestAgentMapping(t *testing.T) {
	alp := &agentLogPoller{}
	r := reporter.NewMock(t)
	config := DefaultConfiguration()
	config.Agents = map[netip.Addr]netip.Addr{
		// Rely on IPv4 → IPv6 conversion in New()
		netip.MustParseAddr("192.0.2.1"): netip.MustParseAddr("192.0.2.10"),
	}
	c := NewMock(t, r, config, Dependencies{Daemon: daemon.NewMock(t)})
	c.poller = alp

	expectSNMPLookup(t, c, "192.0.2.1", 766, answer{NOk: true})
	time.Sleep(20 * time.Millisecond)
	alp.mu.Lock()
	if alp.lastAgent != "192.0.2.10" {
		alp.mu.Unlock()
		t.Fatalf("last agent should have been 192.0.2.10, not %s", alp.lastAgent)
	}
	alp.mu.Unlock()
	expectSNMPLookup(t, c, "192.0.2.2", 766, answer{NOk: true})
	time.Sleep(20 * time.Millisecond)
	alp.mu.Lock()
	if alp.lastAgent != "192.0.2.2" {
		alp.mu.Unlock()
		t.Fatalf("last agent should have been 192.0.2.2, not %s", alp.lastAgent)
	}
	alp.mu.Unlock()
}
