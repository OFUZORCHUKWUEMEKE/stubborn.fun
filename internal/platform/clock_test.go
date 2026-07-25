package platform

import (
	"sync"
	"testing"
	"time"
)

var testBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFakeClock_AdvanceAndSet(t *testing.T) {
	c := NewFakeClock(testBase)

	if got := c.Now(); !got.Equal(testBase) {
		t.Fatalf("Now() = %s, want %s", got, testBase)
	}

	c.Advance(90 * time.Minute)
	if want := testBase.Add(90 * time.Minute); !c.Now().Equal(want) {
		t.Fatalf("after Advance, Now() = %s, want %s", c.Now(), want)
	}

	target := testBase.Add(-24 * time.Hour)
	c.Set(target)
	if !c.Now().Equal(target) {
		t.Fatalf("after Set, Now() = %s, want %s", c.Now(), target)
	}
}

func TestFakeClock_NormalizesToUTC(t *testing.T) {
	loc, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	lagos := time.Date(2026, 1, 1, 12, 0, 0, 0, loc)

	c := NewFakeClock(lagos)
	if c.Now().Location() != time.UTC {
		t.Fatalf("NewFakeClock location = %s, want UTC", c.Now().Location())
	}
	if !c.Now().Equal(lagos) {
		t.Fatalf("UTC normalization changed the instant: %s vs %s", c.Now(), lagos)
	}

	c.Set(lagos)
	if c.Now().Location() != time.UTC {
		t.Fatalf("Set location = %s, want UTC", c.Now().Location())
	}
}

// TestFakeClock_ConcurrentUse matters because schedulers read Now() from
// their own goroutine while a test advances the clock from another. Run
// under -race, this is what proves FakeClock's mutex is doing its job.
func TestFakeClock_ConcurrentUse(t *testing.T) {
	c := NewFakeClock(testBase)

	const readers = 8
	const writers = 4
	const iterations = 200

	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if c.Now().Before(testBase) {
					t.Error("clock went backwards past its start")
					return
				}
			}
		}()
	}
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.Advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	want := testBase.Add(time.Duration(writers*iterations) * time.Millisecond)
	if !c.Now().Equal(want) {
		t.Fatalf("Now() = %s, want %s (lost updates under concurrency)", c.Now(), want)
	}
}

func TestSystemClock_ReturnsUTCNow(t *testing.T) {
	var c Clock = SystemClock{}
	before := time.Now().UTC().Add(-time.Second)
	got := c.Now()
	after := time.Now().UTC().Add(time.Second)

	if got.Location() != time.UTC {
		t.Fatalf("location = %s, want UTC", got.Location())
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %s, outside expected window [%s, %s]", got, before, after)
	}
}
