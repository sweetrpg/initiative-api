package data

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sweetrpg/initiative-api/store"
	"github.com/sweetrpg/initiative-api/store/fake"
	"go.mongodb.org/mongo-driver/bson"
)

// findBarrier holds every FindOne until n callers have found the row, so all of them read the
// ticket before any deletes it: the interleaving a plain run on the fake rarely produces.
type findBarrier struct {
	store.Collection
	wg sync.WaitGroup
}

func (b *findBarrier) FindOne(ctx context.Context, filter bson.D, out any) error {
	err := b.Collection.FindOne(ctx, filter, out)
	b.wg.Done()
	b.wg.Wait()
	return err
}

func TestConcurrentRedeemsOfOneTicketHaveExactlyOneWinner(t *testing.T) {
	const n = 32
	r := NewStreamTickets(fake.NewDB())
	barrier := &findBarrier{Collection: r.c}
	barrier.wg.Add(n)
	if err := r.Mint(context.Background(), StreamTicket{ID: "hash", UserID: "u-1", SessionID: "s", Level: ViewPlayer}); err != nil {
		t.Fatal(err)
	}
	r.c = barrier
	var won, refused atomic.Int32
	var wg, start sync.WaitGroup
	start.Add(1)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			switch _, err := r.Redeem(context.Background(), "hash", time.Minute); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, ErrNotFound):
				refused.Add(1)
			default:
				t.Errorf("redeem: %v", err)
			}
		}()
	}
	start.Done()
	wg.Wait()
	if won.Load() != 1 || refused.Load() != n-1 {
		t.Errorf("%d redeems won and %d were refused, want exactly 1 and %d", won.Load(), refused.Load(), n-1)
	}
}

func TestAnExpiredTicketIsSpentAndRefused(t *testing.T) {
	r := NewStreamTickets(fake.NewDB())
	now := time.Now()
	r.now = func() time.Time { return now }
	_ = r.Mint(context.Background(), StreamTicket{ID: "hash", UserID: "u-1"})
	r.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := r.Redeem(context.Background(), "hash", 30*time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired redeem = %v, want ErrNotFound", err)
	}
	if _, err := r.Redeem(context.Background(), "hash", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired ticket stayed redeemable: %v", err)
	}
}
