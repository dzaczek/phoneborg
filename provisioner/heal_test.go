package provisioner

import "testing"

func TestParseAdvertisePort(t *testing.T) {
	cases := map[string]int{
		"./node-agent\x00--controller\x00http://127.0.0.1:18080\x00--advertise-port\x0054495\x00": 54495,
		"./node-agent --advertise-port 40123 --serve-port 18090":                                  40123,
		"./node-agent -advertise-port=40124":                                                      40124,
	}
	for in, want := range cases {
		if got, ok := ParseAdvertisePort(in); !ok || got != want {
			t.Errorf("%q = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "./node-agent --controller x", "./node-agent --advertise-port", "./node-agent --advertise-port abc"} {
		if _, ok := ParseAdvertisePort(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}

func TestReverseListed(t *testing.T) {
	if !ReverseListed("UsbFfs tcp:18080 tcp:18080\n", 18080) {
		t.Fatal("listed reverse not found")
	}
	if !ReverseListed("host-13 tcp:18080 tcp:18080\n", 18080) {
		t.Fatal("tcp transport reverse not found")
	}
	if ReverseListed("", 18080) || ReverseListed("UsbFfs tcp:8080 tcp:8080\n", 18080) {
		t.Fatal("missing reverse reported as present")
	}
}
