package codexauth

import "testing"

func TestNativeLimitsDecodeUnknownAndMultipleBuckets(t *testing.T) {
	for _, raw := range []string{`{}`, `{"rateLimits":null}`, `{"rateLimits":{"primary":{}}}`} {
		q, e := decodeLimits([]byte(raw), 10)
		if e != nil || q.State != "unknown" || len(q.Windows) != 0 {
			t.Fatalf("missing means unknown: %+v %v", q, e)
		}
	}
	q, e := decodeLimits([]byte(`{"rateLimits":{"primary":{"usedPercent":99}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":123}},"other":{"secondary":{"usedPercent":50}}}}`), 10)
	if e != nil || len(q.Windows) != 2 || q.Windows[0].UsedPercent != 0 || q.Windows[1].UsedPercent != 50 || q.ObservedAt != 10 {
		t.Fatalf("%+v %v", q, e)
	}
	if _, e = decodeLimits([]byte(`{"rateLimits":{"primary":{"usedPercent":101}}}`), 0); e == nil {
		t.Fatal("invalid percent accepted")
	}
}
