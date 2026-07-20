package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

type concurrentQueue struct {
	mu    sync.Mutex
	items []OutboxItem
}

func (*concurrentQueue) RenewOutboxLease(context.Context, string, string, time.Duration) error {
	return nil
}

func (q *concurrentQueue) ClaimOutbox(context.Context, string, time.Duration) (OutboxItem, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return OutboxItem{}, false, nil
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true, nil
}

func (*concurrentQueue) CompleteOutbox(context.Context, string) error { return nil }
func (*concurrentQueue) RetryOutbox(context.Context, string, string, time.Time) error {
	return nil
}

func TestWorkerRunsIndependentOutboxItemsConcurrently(t *testing.T) {
	queue := &concurrentQueue{items: []OutboxItem{{ID: "1", Kind: "work"}, {ID: "2", Kind: "work"}}}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	worker := NewWorker(queue, map[string]Handler{"work": func(context.Context, OutboxItem) error {
		started <- struct{}{}
		<-release
		return nil
	}}, "test", time.Millisecond, nil)
	worker.SetConcurrency(2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	for index := 0; index < 2; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("second independent item did not start concurrently")
		}
	}
	close(release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

type renewingQueue struct {
	concurrentQueue
	renewed chan struct{}
}

func (q *renewingQueue) RenewOutboxLease(context.Context, string, string, time.Duration) error {
	select {
	case q.renewed <- struct{}{}:
	default:
	}
	return nil
}

func TestWorkerRenewsLeaseWhileHandlerIsActive(t *testing.T) {
	queue := &renewingQueue{renewed: make(chan struct{}, 1)}
	release := make(chan struct{})
	worker := NewWorker(queue, nil, "worker", time.Millisecond, nil)
	worker.SetLease(300 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		done <- worker.handleWithLease(context.Background(), OutboxItem{ID: "item"}, func(context.Context, OutboxItem) error {
			<-release
			return nil
		})
	}()
	select {
	case <-queue.renewed:
	case <-time.After(time.Second):
		t.Fatal("long-running handler lease was not renewed")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
