package batch

import (
	"context"
	"math/rand"
	"slices"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

func TestBatch(t *testing.T) {
	want := 1000
	results := []int{}
	batches := 0

	w := New(func(_ context.Context, input []int) error {
		t.Logf("batch size: %d", len(input))
		batches++
		for _, i := range input {
			results = append(results, i)
		}

		// Pretend this is slow.
		time.Sleep(time.Duration(rand.Intn(want/10)) * time.Microsecond)

		return nil
	})

	var g errgroup.Group
	for i := range want {
		i := i
		g.Go(func() error {
			return w.Do(context.Background(), i)
		})
	}

	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}

	if got := len(results); got != want {
		t.Errorf("got %d, want %d", got, want)
	}

	slices.Sort(results)

	for i := range want {
		if results[i] != i {
			t.Errorf("results[%d] != %d", results[i], i)
		}
	}

	if batches <= 1 || batches >= want {
		t.Errorf("pathological number of batches: %d", batches)
	}
}
