package access

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"
)

// replayBenchAuthority opens an authority with one session per subject and
// fills its replay ledger with ledger accepted requests.
func replayBenchAuthority(b *testing.B, subjects, ledger int) (*FileAuthority, []Credential, time.Time) {
	b.Helper()
	now := time.Unix(1_800_000_000, 0)
	a, err := open(b.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = a.Close() })
	creds := make([]Credential, subjects)
	for i := range creds {
		dir, err := a.Ensure(context.Background(), SubjectSession, fmt.Sprintf("s%03d", i))
		if err != nil {
			b.Fatal(err)
		}
		if creds[i], err = LoadCredential(dir); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < ledger; i++ {
		req, err := SignRequest(creds[i%subjects], a.BootID(), []byte(`{"query":"snapshot"}`), now, rand.Reader)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := a.Verify(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
	return a, creds, now
}

// BenchmarkVerifyLedgerSize measures one accepted request with a ledger of
// the given size already persisted.
func BenchmarkVerifyLedgerSize(b *testing.B) {
	for _, ledger := range []int{0, 1000, 3500} {
		b.Run(fmt.Sprintf("ledger=%d", ledger), func(b *testing.B) {
			a, creds, now := replayBenchAuthority(b, 1, ledger)
			reqs := make([]SignedRequest, b.N)
			for i := range reqs {
				reqs[i], _ = SignRequest(creds[0], a.BootID(), []byte(`{"query":"snapshot"}`), now, rand.Reader)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := a.Verify(context.Background(), reqs[i]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkVerifyConcurrentSubjects measures accepted requests per second
// with one goroutine per subject verifying concurrently, over a ledger of
// 1000 entries.
func BenchmarkVerifyConcurrentSubjects(b *testing.B) {
	for _, subjects := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("subjects=%d", subjects), func(b *testing.B) {
			a, creds, now := replayBenchAuthority(b, subjects, 1000)
			per := (b.N + subjects - 1) / subjects
			reqs := make([][]SignedRequest, subjects)
			for s := range reqs {
				reqs[s] = make([]SignedRequest, per)
				for i := range reqs[s] {
					reqs[s][i], _ = SignRequest(creds[s], a.BootID(), []byte(`{"query":"snapshot"}`), now, rand.Reader)
				}
			}
			b.ResetTimer()
			start := time.Now()
			var wg sync.WaitGroup
			for s := 0; s < subjects; s++ {
				wg.Add(1)
				go func(s int) {
					defer wg.Done()
					for _, req := range reqs[s] {
						if _, err := a.Verify(context.Background(), req); err != nil {
							b.Error(err)
							return
						}
					}
				}(s)
			}
			wg.Wait()
			b.ReportMetric(float64(per*subjects)/time.Since(start).Seconds(), "verifies/s")
		})
	}
}
