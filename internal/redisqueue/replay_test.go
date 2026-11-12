package redisqueue

import (
	"bytes"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestUsagePayloadShapes(t *testing.T) {
	for _, payload := range []string{`{"id":1}`, "usage-record", `[1]`, `"text"`, `null`, "{invalid", "\x00\xff"} {
		t.Run(payload, func(t *testing.T) {
			withEnabledQueue(t, func() {
				start := ReadAfter(0, 1).NextSeq
				Enqueue([]byte(payload))
				popped := PopOldest(1)[0]
				subscriber, unsubscribe := SubscribeUsage()
				defer unsubscribe()
				requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
				Enqueue([]byte(payload))
				streamed := <-subscriber
				page := ReadAfter(start, 10)
				if len(page.Records) != 2 || !bytes.Equal(page.Records[0].Payload, popped) || !bytes.Equal(page.Records[1].Payload, streamed) {
					t.Fatal("replay differs from pop/subscribe")
				}
				for i, delivered := range [][]byte{popped, streamed} {
					if payload == `{"id":1}` {
						if ojson.GetBytes(delivered, "id").Int() != 1 || ojson.GetBytes(delivered, "queue_seq").Uint() != start+uint64(i)+1 || ojson.GetBytes(delivered, "boot_id").String() != page.BootID || page.BootID == "" {
							t.Fatalf("missing object metadata: %s", delivered)
						}
					} else if string(delivered) != payload {
						t.Fatalf("non-object changed: %q", delivered)
					}
				}
				if got := PopOldest(10); len(got) != 0 {
					t.Fatal("streamed record entered pop queue")
				}
			})
		})
	}
}

func TestUsageObjectDeliveryOwnsPayload(t *testing.T) {
	for _, subscribe := range []bool{false, true} {
		withEnabledQueue(t, func() {
			var subscriber <-chan []byte
			if subscribe {
				var unsubscribe func()
				subscriber, unsubscribe = SubscribeUsage()
				defer unsubscribe()
				requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
			}
			start := ReadAfter(0, 1).NextSeq
			payload := []byte(`{"id":"original"}`)
			Enqueue(payload)
			want := ReadAfter(start, 1).Records[0].Payload
			for i := range payload {
				payload[i] = 'x'
			}
			var delivered []byte
			if subscribe {
				delivered = <-subscriber
			} else {
				delivered = PopOldest(1)[0]
			}
			if !bytes.Equal(delivered, want) || !bytes.Equal(ReadAfter(start, 1).Records[0].Payload, want) {
				t.Fatalf("caller mutation changed delivery (subscribe=%v)", subscribe)
			}
		})
	}
}

func TestUsageSequenceConcurrentEnqueue(t *testing.T) {
	withEnabledQueue(t, func() {
		var fresh queue
		fresh.enqueue([]byte("first"))
		if fresh.items[0].sequence != 1 {
			t.Fatal("sequence did not start at one")
		}
		start := ReadAfter(0, 1).NextSeq
		subscriber, unsubscribe := SubscribeUsage()
		defer unsubscribe()
		requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 16 {
					Enqueue([]byte(`{"id":1}`))
				}
			})
		}
		wg.Wait()
		page := ReadAfter(start, 128)
		if len(page.Records) != 128 {
			t.Fatalf("record count = %d", len(page.Records))
		}
		for i, record := range page.Records {
			if record.QueueSeq != start+uint64(i)+1 || !bytes.Equal(record.Payload, <-subscriber) {
				t.Fatalf("sequence/stream order differs at %d", i)
			}
		}
	})
}

func TestUsageReplayRepeatableAndIndependentOfPop(t *testing.T) {
	withEnabledQueue(t, func() {
		start := ReadAfter(0, 1).NextSeq
		for _, payload := range []string{"a", "b", "c"} {
			Enqueue([]byte(payload))
		}
		first := ReadAfter(start, 2)
		if !reflect.DeepEqual(first, ReadAfter(start, 2)) || first.NextSeq != start+2 {
			t.Fatal("replay is not repeatable/paginated")
		}
		popped := PopOldest(3)
		if len(popped) != 3 {
			t.Fatal("replay consumed pop records")
		}
		popped[0][0] = 'x'
		if !reflect.DeepEqual(first, ReadAfter(start, 2)) {
			t.Fatal("pop changed replay")
		}
		first.Records[0].Payload[0] = 'x'
		if string(ReadAfter(start, 1).Records[0].Payload) != "a" {
			t.Fatal("reader changed replay")
		}
		last := ReadAfter(first.NextSeq, 2)
		if len(last.Records) != 1 || string(last.Records[0].Payload) != "c" || last.NextSeq != start+3 {
			t.Fatal("cursor did not advance to last record")
		}
	})
}

func TestUsageReplayRetentionAndLateCursor(t *testing.T) {
	withEnabledQueue(t, func() {
		now := time.Unix(100, 0)
		global.nowFunc = func() time.Time { return now }
		previous := retentionSeconds.Load()
		SetRetentionSeconds(10)
		defer func() { global.nowFunc = nil; retentionSeconds.Store(previous) }()
		start := ReadAfter(0, 1).NextSeq
		Enqueue([]byte("old"))
		now = now.Add(10 * time.Second)
		Enqueue([]byte("held"))
		if len(ReadAfter(start, 10).Records) != 2 {
			t.Fatal("record at exact cutoff was pruned")
		}
		now = now.Add(time.Nanosecond)
		page := ReadAfter(start, 10)
		if len(page.Records) != 1 || string(page.Records[0].Payload) != "held" || page.OldestSeq-start-1 != 1 {
			t.Fatalf("late cursor did not report one missed record: %+v", page)
		}
		now = now.Add(10 * time.Second)
		page = ReadAfter(start, 10)
		if len(page.Records) != 0 || page.OldestSeq != start+3 || page.NextSeq != start+2 {
			t.Fatalf("fully pruned log lost its cursor/gap: %+v", page)
		}
	})
}

func TestUsageReplayCapsEvictOldest(t *testing.T) {
	for _, tc := range []struct {
		name                string
		size, count, missed int
	}{
		{"records", 1, maxReplayRecords + 2, 2},
		{"bytes", maxReplayBytes / 2, 3, 1},
		{"oversized", maxReplayBytes + 1, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withEnabledQueue(t, func() {
				start := ReadAfter(0, 1).NextSeq
				payload := bytes.Repeat([]byte("x"), tc.size)
				for range tc.count {
					Enqueue(payload)
				}
				page := ReadAfter(start, 1)
				if page.OldestSeq-start-1 != uint64(tc.missed) || global.replay.bytes > maxReplayBytes || len(global.replay.items)-global.replay.head > maxReplayRecords {
					t.Fatalf("incorrect gap or cap: oldest=%d start=%d bytes=%d", page.OldestSeq, start, global.replay.bytes)
				}
				if tc.count > tc.missed && (len(page.Records) != 1 || page.Records[0].QueueSeq != page.OldestSeq) {
					t.Fatal("cap did not evict oldest first")
				}
			})
		})
	}
}

func TestUsageReplaySurvivesSubscriberOverflow(t *testing.T) {
	withEnabledQueue(t, func() {
		start := ReadAfter(0, 1).NextSeq
		_, unsubscribe := SubscribeUsage()
		defer unsubscribe()
		for range usageSubscriberBuffer + 1 {
			Enqueue([]byte("record"))
		}
		if got := ReadAfter(start, usageSubscriberBuffer+1); len(got.Records) != usageSubscriberBuffer+1 {
			t.Fatal("subscriber overflow lost replay records")
		}
	})
}

func TestUsageReplayDisableAndControls(t *testing.T) {
	withEnabledQueue(t, func() {
		start := ReadAfter(0, 1)
		subscriber, unsubscribe := SubscribeUsage()
		defer unsubscribe()
		requireUsageSubscriberPayload(t, subscriber, usageSupportRefreshPayload)
		NotifyUsageRefresh()
		requireUsageSubscriberPayload(t, subscriber, usageRefreshPayload)
		EnqueueError([]byte("error"))
		Enqueue(nil)
		if !reflect.DeepEqual(start, ReadAfter(0, 1)) {
			t.Fatal("control/error/empty payload consumed a sequence")
		}
		Enqueue([]byte("record"))
		SetEnabled(false)
		Enqueue([]byte("disabled"))
		page := ReadAfter(start.NextSeq, 10)
		if len(page.Records) != 0 || page.BootID != start.BootID || page.NextSeq != start.NextSeq+1 || global.replay.bytes != 0 {
			t.Fatal("disable did not clear replay or changed process identity/sequence")
		}
		SetEnabled(true)
		Enqueue([]byte("enabled"))
		if ReadAfter(page.NextSeq, 1).NextSeq != page.NextSeq+1 {
			t.Fatal("sequence reset after enable")
		}
	})
}
