package redisqueue

import "testing"

func TestUsageQueueLegacyDelivery(t *testing.T) {
	withEnabledQueue(t, func() {
		Enqueue([]byte("before-subscribe"))
		subscriber, unsubscribe := SubscribeUsage()
		requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
		select {
		case got := <-subscriber:
			t.Fatalf("subscribe replayed queued payload %q", got)
		default:
		}
		Enqueue([]byte("streamed"))
		requireUsageSubscriberPayload(t, subscriber, "streamed")
		items := PopOldest(10)
		if len(items) != 1 || string(items[0]) != "before-subscribe" {
			t.Fatalf("pop = %q, want only pre-subscription payload", items)
		}
		if got := PopOldest(10); len(got) != 0 {
			t.Fatalf("pop was not destructive: %q", got)
		}
		unsubscribe()
		Enqueue([]byte("after-unsubscribe"))
		if got := PopOldest(1); len(got) != 1 || string(got[0]) != "after-unsubscribe" {
			t.Fatalf("pop after unsubscribe = %q", got)
		}
	})
}

func TestUsageQueueOverflowDropsSubscriberWithoutQueuingTrigger(t *testing.T) {
	withEnabledQueue(t, func() {
		subscriber, unsubscribe := SubscribeUsage()
		defer unsubscribe()
		requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
		for range usageSubscriberBuffer {
			Enqueue([]byte("buffered"))
		}
		Enqueue([]byte("overflow"))
		if got := PopOldest(10); len(got) != 0 {
			t.Fatalf("overflow trigger entered pop queue: %q", got)
		}
		for range usageSubscriberBuffer {
			requireUsageSubscriberPayload(t, subscriber, "buffered")
		}
		if _, ok := <-subscriber; ok {
			t.Fatal("overflowed subscriber is still open")
		}
		Enqueue([]byte("next"))
		if got := PopOldest(1); len(got) != 1 || string(got[0]) != "next" {
			t.Fatalf("pop after overflow = %q", got)
		}
	})
}
