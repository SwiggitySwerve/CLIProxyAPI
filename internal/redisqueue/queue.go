package redisqueue

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	defaultRetentionSeconds int64 = 60
	maxRetentionSeconds     int64 = 3600
	usageSubscriberBuffer         = 256
	errorSubscriberBuffer         = 256

	maxReplayRecords = 16_384
	maxReplayBytes   = 16 << 20 // Bound retained payloads independently of record size/rate.

	usageSupportRefreshPayload = `{"support_refresh":true}`
	usageRefreshPayload        = `{"refresh":true}`
)

type queueItem struct {
	enqueuedAt time.Time
	payload    []byte
	sequence   uint64
}

type itemBuffer struct {
	items []queueItem
	head  int
	bytes int
}

type queue struct {
	itemBuffer
	mu               sync.Mutex
	subscribers      map[uint64]chan []byte
	nextSubscriberID uint64
	replay           itemBuffer
	nextSequence     uint64
	nowFunc          func() time.Time
}

var (
	enabled          atomic.Bool
	retentionSeconds atomic.Int64
	global           queue
	errorGlobal      queue
	bootID           = uuid.NewString()
)

func init() {
	retentionSeconds.Store(defaultRetentionSeconds)
}

func SetEnabled(value bool) {
	enabled.Store(value)
	if !value {
		global.clear()
		errorGlobal.clear()
	}
}

func Enabled() bool {
	return enabled.Load()
}

func SetRetentionSeconds(value int) {
	normalized := int64(value)
	if normalized <= 0 {
		normalized = defaultRetentionSeconds
	} else if normalized > maxRetentionSeconds {
		normalized = maxRetentionSeconds
	}
	retentionSeconds.Store(normalized)
}

func Enqueue(payload []byte) {
	if !Enabled() {
		return
	}
	if len(payload) == 0 {
		return
	}
	global.enqueue(payload)
}

func EnqueueError(payload []byte) {
	if !Enabled() {
		return
	}
	if len(payload) == 0 {
		return
	}
	errorGlobal.publishToSubscribers(payload)
}

func PopOldest(count int) [][]byte {
	if !Enabled() {
		return nil
	}
	if count <= 0 {
		return nil
	}
	return global.popOldest(count)
}

func SubscribeUsage() (<-chan []byte, func()) {
	return global.subscribe(usageSubscriberBuffer, []byte(usageSupportRefreshPayload))
}

func SubscribeErrors() (<-chan []byte, func()) {
	return errorGlobal.subscribe(errorSubscriberBuffer, nil)
}

func NotifyUsageRefresh() {
	global.publishToSubscribers([]byte(usageRefreshPayload))
}

func (q *queue) clear() {
	q.mu.Lock()

	subscribers := make([]chan []byte, 0, len(q.subscribers))
	for _, subscriber := range q.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	q.items = nil
	q.head = 0
	q.bytes = 0
	q.replay = itemBuffer{}
	q.subscribers = nil
	q.mu.Unlock()

	for _, subscriber := range subscribers {
		close(subscriber)
	}
}

func (q *queue) enqueue(payload []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !Enabled() {
		return
	}
	now := q.now()
	sequence := q.nextSequence + 1
	if ojson.ValidBytes(payload) && ojson.ParseBytes(payload).IsObject() {
		doc := ojson.NewDoc(payload)
		if err := doc.Set("queue_seq", sequence); err != nil {
			log.Errorf("usage queue sequence: %v", err)
			return
		}
		if err := doc.Set("boot_id", bootID); err != nil {
			log.Errorf("usage queue boot id: %v", err)
			return
		}
		payload = doc.Bytes()
	} else {
		payload = append([]byte(nil), payload...)
	}
	q.nextSequence = sequence
	item := queueItem{enqueuedAt: now, payload: payload, sequence: sequence}
	q.replay.pruneLocked(now)
	q.replay.appendLocked(item)
	for len(q.replay.items)-q.replay.head > maxReplayRecords || q.replay.bytes > maxReplayBytes {
		q.replay.dropOldestLocked()
	}
	q.replay.maybeCompactLocked()

	q.pruneLocked(now)
	if !q.publishLocked(payload) {
		q.appendLocked(item)
	}
	q.maybeCompactLocked()
}

func (q *queue) now() time.Time {
	if q.nowFunc != nil {
		return q.nowFunc()
	}
	return time.Now()
}

func (q *queue) publishToSubscribers(payload []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.publishLocked(payload)
}

func (q *queue) publishLocked(payload []byte) bool {
	if len(q.subscribers) == 0 {
		return false
	}

	for id, subscriber := range q.subscribers {
		cloned := append([]byte(nil), payload...)
		select {
		case subscriber <- cloned:
		default:
			delete(q.subscribers, id)
			close(subscriber)
		}
	}

	return true
}

func (q *queue) subscribe(buffer int, initialPayload []byte) (<-chan []byte, func()) {
	subscriber := make(chan []byte, buffer)
	if len(initialPayload) > 0 {
		subscriber <- append([]byte(nil), initialPayload...)
	}

	q.mu.Lock()
	if q.subscribers == nil {
		q.subscribers = make(map[uint64]chan []byte)
	}
	q.nextSubscriberID++
	id := q.nextSubscriberID
	q.subscribers[id] = subscriber
	q.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			q.unsubscribe(id)
		})
	}
	return subscriber, unsubscribe
}

func (q *queue) unsubscribe(id uint64) {
	q.mu.Lock()
	subscriber, ok := q.subscribers[id]
	if ok {
		delete(q.subscribers, id)
	}
	q.mu.Unlock()

	if ok {
		close(subscriber)
	}
}

func (q *queue) popOldest(count int) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(q.now())
	available := len(q.items) - q.head
	if available <= 0 {
		q.items = nil
		q.head = 0
		return nil
	}
	if count > available {
		count = available
	}

	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, append([]byte(nil), q.items[q.head].payload...))
		q.dropOldestLocked()
	}
	q.maybeCompactLocked()
	return out
}

// ReplayPage is a snapshot of the retained log, independent of pop/subscription state.
type ReplayPage struct {
	BootID    string
	OldestSeq uint64
	NextSeq   uint64
	Records   []ReplayRecord
}

type ReplayRecord struct {
	QueueSeq uint64
	Payload  []byte
}

// ReadAfter returns at most count records after the cursor, without removing them.
// An empty log has OldestSeq equal to the last assigned sequence plus one.
func ReadAfter(after uint64, count int) ReplayPage {
	global.mu.Lock()
	defer global.mu.Unlock()
	q := &global
	q.replay.pruneLocked(q.now())
	q.replay.maybeCompactLocked()
	page := ReplayPage{BootID: bootID, OldestSeq: q.nextSequence + 1, NextSeq: after}
	if q.replay.head < len(q.replay.items) {
		page.OldestSeq = q.replay.items[q.replay.head].sequence
	}
	// Advance over pruned records even if none remain, so they are counted once.
	if page.NextSeq < page.OldestSeq-1 {
		page.NextSeq = page.OldestSeq - 1
	}
	for _, item := range q.replay.items[q.replay.head:] {
		if item.sequence <= after {
			continue
		}
		if len(page.Records) >= count {
			break
		}
		page.Records = append(page.Records, ReplayRecord{QueueSeq: item.sequence, Payload: append([]byte(nil), item.payload...)})
		page.NextSeq = item.sequence
	}
	return page
}

func (q *itemBuffer) appendLocked(item queueItem) {
	q.items = append(q.items, item)
	q.bytes += len(item.payload)
}

func (q *itemBuffer) dropOldestLocked() {
	q.bytes -= len(q.items[q.head].payload)
	q.items[q.head] = queueItem{}
	q.head++
}

func (q *itemBuffer) pruneLocked(now time.Time) {
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}

	windowSeconds := retentionSeconds.Load()
	if windowSeconds <= 0 {
		windowSeconds = defaultRetentionSeconds
	}
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second)
	for q.head < len(q.items) && q.items[q.head].enqueuedAt.Before(cutoff) {
		q.dropOldestLocked()
	}
}

func (q *itemBuffer) maybeCompactLocked() {
	if q.head == 0 {
		return
	}
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}
	if q.head < 1024 && q.head*2 < len(q.items) {
		return
	}
	q.items = append([]queueItem(nil), q.items[q.head:]...)
	q.head = 0
}
