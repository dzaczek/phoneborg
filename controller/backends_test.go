package controller

import (
	"testing"

	"github.com/dzaczek/phoneborg/proto"
)

func TestBackendsOnlyReadyActiveNodes(t *testing.T) {
	rt := func(ready bool, host string) *proto.Heartbeat {
		return &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "m", Ready: ready, AdvertiseHost: host, AdvertisePort: 4000}}
	}
	nodes := []proto.Node{
		{ID: "ok", State: proto.StateActive, LastHeartbeat: rt(true, "")},
		{ID: "wifi", State: proto.StateActive, LastHeartbeat: rt(true, "10.0.0.7")},
		{ID: "loading", State: proto.StateActive, LastHeartbeat: rt(false, "")},
		{ID: "suspect", State: proto.StateSuspect, LastHeartbeat: rt(true, "")},
		{ID: "no-runtime", State: proto.StateActive, LastHeartbeat: &proto.Heartbeat{}},
		{ID: "no-heartbeat", State: proto.StateActive},
	}
	got := Backends(nodes, map[string]bool{"wifi": true}, "host.docker.internal", 0)
	if len(got) != 2 || got[0].URL != "http://host.docker.internal:4000" || got[1].URL != "http://10.0.0.7:4000" ||
		got[0].Drained || !got[1].Drained {
		t.Fatalf("backends = %+v", got)
	}
}

func TestBackendsSpeedPrefersMeasuredGenTPS(t *testing.T) {
	rt := func(genTPS float64) *proto.Heartbeat {
		return &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 4000, GenTPS: genTPS}}
	}
	nodes := []proto.Node{
		{ID: "measured", State: proto.StateActive, LastHeartbeat: rt(12.5), Benchmark: &proto.Benchmark{CPUGFLOPS: 7.4}},
		{ID: "unmeasured", State: proto.StateActive, LastHeartbeat: rt(0), Benchmark: &proto.Benchmark{CPUGFLOPS: 8.6}},
	}
	got := Backends(nodes, nil, "", 0)
	speed := map[string]float64{}
	for _, b := range got {
		speed[b.NodeID] = b.Speed
	}
	// Once any node reports a measured speed, the unmeasured one is not
	// compared on the benchmark's different scale: it gets 0, not 8.6.
	if speed["measured"] != 12.5 || speed["unmeasured"] != 0 {
		t.Fatalf("speed = %+v", speed)
	}
}

func TestBackendsSpeedFallsBackToBenchmarkWhenNoneMeasured(t *testing.T) {
	rt := &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 4000}}
	nodes := []proto.Node{{ID: "a", State: proto.StateActive, LastHeartbeat: rt, Benchmark: &proto.Benchmark{CPUGFLOPS: 8.6}}}
	got := Backends(nodes, nil, "", 0)
	if len(got) != 1 || got[0].Speed != 8.6 {
		t.Fatalf("backends = %+v", got)
	}
}

func TestBackendsHotAboveThermalLimit(t *testing.T) {
	temp := func(c float64) *proto.Heartbeat {
		return &proto.Heartbeat{TemperatureC: &c, Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 4000}}
	}
	nodes := []proto.Node{
		{ID: "hot", State: proto.StateActive, LastHeartbeat: temp(80)},
		{ID: "warm", State: proto.StateActive, LastHeartbeat: temp(74.9)},
		{ID: "unknown", State: proto.StateActive, LastHeartbeat: &proto.Heartbeat{Runtime: &proto.RuntimeStatus{Model: "m", Ready: true, AdvertisePort: 4000}}},
	}
	got := Backends(nodes, nil, "", 75)
	hot := map[string]bool{}
	for _, b := range got {
		hot[b.NodeID] = b.Hot
	}
	if hot["hot"] != true || hot["warm"] != false || hot["unknown"] != false {
		t.Fatalf("hot = %+v", hot)
	}
	// thermalLimitC=0 disables thermal marking entirely.
	got = Backends(nodes, nil, "", 0)
	for _, b := range got {
		if b.Hot {
			t.Fatalf("thermal limit 0 should disable Hot: %+v", b)
		}
	}
}
