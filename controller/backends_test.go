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
	got := Backends(nodes, map[string]bool{"wifi": true}, "host.docker.internal")
	if len(got) != 2 || got[0].URL != "http://host.docker.internal:4000" || got[1].URL != "http://10.0.0.7:4000" ||
		got[0].Drained || !got[1].Drained {
		t.Fatalf("backends = %+v", got)
	}
}
