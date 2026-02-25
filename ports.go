package main

import (
	"fmt"
	"hash/fnv"
	"sort"
)

const (
	portRangeMin = 10000
	portRangeMax = 65535
	portRange    = portRangeMax - portRangeMin + 1
)

// hashPort computes a deterministic port for a host:tenant pair.
func hashPort(host, tenant string) uint16 {
	h := fnv.New32a()
	fmt.Fprintf(h, "%s:%s", host, tenant)
	return uint16(portRangeMin + int(h.Sum32())%portRange)
}

// AssignPorts assigns unique deterministic local ports to a list of entries.
// Entries are sorted by host:tenant for deterministic collision resolution.
func AssignPorts(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		ki := entries[i].Host + ":" + entries[i].Tenant
		kj := entries[j].Host + ":" + entries[j].Tenant
		return ki < kj
	})

	used := make(map[uint16]bool)
	for i := range entries {
		port := hashPort(entries[i].Host, entries[i].Tenant)
		for used[port] {
			port++
			if port > portRangeMax {
				port = portRangeMin
			}
		}
		used[port] = true
		entries[i].LocalPort = port
	}
}
