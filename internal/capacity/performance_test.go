package capacity

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkEvaluate(b *testing.B) {
	for _, n := range []int32{3, 100} {
		for _, intermediate := range []bool{false, true} {
			b.Run(fmt.Sprintf("replicas=%d/intermediate=%t", n, intermediate), func(b *testing.B) {
				p := policy()
				p.MaxReplicas = 100
				now := time.Unix(10000, 0)
				old := Evaluate(p, State{}, observation(now, n, 200), n)
				delay := 15 * time.Second
				if intermediate {
					delay = 5 * time.Second
				}
				o := observation(now.Add(delay), n, 200)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					_ = Evaluate(p, old, o, n)
				}
			})
		}
	}
}

func BenchmarkLowDemand(b *testing.B) {
	p := policy()
	p.MaxReplicas = 100
	o := observation(time.Unix(10000, 0), 100, 10)
	b.ReportAllocs()
	for b.Loop() {
		_ = LowDemand(p, o, 100)
	}
}
