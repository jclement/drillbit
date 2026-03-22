package main

import (
	"testing"
)

func TestHashPort(t *testing.T) {
	// Deterministic: same input always gives same output.
	port1 := hashPort("server1", "db1")
	port2 := hashPort("server1", "db1")
	if port1 != port2 {
		t.Errorf("hashPort is not deterministic: %d != %d", port1, port2)
	}

	// In range.
	if port1 < portRangeMin || port1 > portRangeMax {
		t.Errorf("port %d out of range [%d, %d]", port1, portRangeMin, portRangeMax)
	}

	// Different inputs give different ports (usually).
	port3 := hashPort("server1", "db2")
	if port1 == port3 {
		t.Log("warning: hash collision between server1/db1 and server1/db2 (unlikely but possible)")
	}
}

func TestAssignPorts(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		entries1 := []Entry{
			{Host: "server1", Container: "db1"},
			{Host: "server1", Container: "db2"},
			{Host: "server2", Container: "db1"},
		}
		entries2 := []Entry{
			{Host: "server1", Container: "db1"},
			{Host: "server1", Container: "db2"},
			{Host: "server2", Container: "db1"},
		}
		AssignPorts(entries1)
		AssignPorts(entries2)

		for i := range entries1 {
			if entries1[i].LocalPort != entries2[i].LocalPort {
				t.Errorf("entry %d: port %d != %d", i, entries1[i].LocalPort, entries2[i].LocalPort)
			}
		}
	})

	t.Run("unique ports", func(t *testing.T) {
		entries := []Entry{
			{Host: "server1", Container: "db1"},
			{Host: "server1", Container: "db2"},
			{Host: "server2", Container: "db1"},
			{Host: "server2", Container: "db2"},
			{Host: "server3", Container: "db1"},
		}
		AssignPorts(entries)

		seen := make(map[uint16]bool)
		for _, e := range entries {
			if seen[e.LocalPort] {
				t.Errorf("duplicate port %d", e.LocalPort)
			}
			seen[e.LocalPort] = true

			if e.LocalPort < portRangeMin || e.LocalPort > portRangeMax {
				t.Errorf("port %d out of range", e.LocalPort)
			}
		}
	})

	t.Run("empty", func(t *testing.T) {
		// Should not panic.
		AssignPorts(nil)
		AssignPorts([]Entry{})
	})

	t.Run("order independent", func(t *testing.T) {
		entries1 := []Entry{
			{Host: "server1", Container: "db1"},
			{Host: "server2", Container: "db2"},
		}
		entries2 := []Entry{
			{Host: "server2", Container: "db2"},
			{Host: "server1", Container: "db1"},
		}
		AssignPorts(entries1)
		AssignPorts(entries2)

		// After sorting, same host:container should have same port.
		ports1 := make(map[string]uint16)
		for _, e := range entries1 {
			ports1[e.Host+":"+e.Container] = e.LocalPort
		}
		for _, e := range entries2 {
			key := e.Host + ":" + e.Container
			if ports1[key] != e.LocalPort {
				t.Errorf("%s: port %d != %d", key, e.LocalPort, ports1[key])
			}
		}
	})
}
