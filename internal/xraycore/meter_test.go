package xraycore

import "testing"

func TestParseNonResettingStatisticsAttribution(t *testing.T) {
	data := []byte(`{"stat":[{"name":"user>>>vless.abc.def>>>traffic>>>uplink","value":"123"},{"name":"user>>>trojan.abc.def>>>traffic>>>downlink","value":456},{"name":"user>>>vless.abc.def>>>traffic>>>downlink"}]}`)
	counters, err := parseCounters(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(counters) != 2 || counters[0].BindingID != "trojan" || counters[0].Uplink != nil || *counters[0].Downlink != 456 || counters[1].BindingID != "vless" || *counters[1].Uplink != 123 || *counters[1].Downlink != 0 {
		t.Fatal("statistics attribution lost")
	}
	for _, value := range []string{"-1", "9223372036854775808", "1.1"} {
		if _, err = parseCounters([]byte(`{"stat":[{"name":"user>>>vless.abc.def>>>traffic>>>uplink","value":` + value + `}]}`)); err == nil {
			t.Fatal("invalid byte count accepted", value)
		}
	}
	if _, err = parseCounters([]byte(`{"stat":[{"name":"user>>>ambiguous.subject>>>traffic>>>uplink","value":1}]}`)); err == nil {
		t.Fatal("ambiguous period attribution accepted")
	}
	if _, err = parseCounters([]byte(`{"stat":[{"name":"user>>>vless.abc.def>>>traffic>>>uplink","value":1},{"name":"user>>>vless.abc.def>>>traffic>>>uplink","value":2}]}`)); err == nil {
		t.Fatal("duplicate counter accepted")
	}
}
