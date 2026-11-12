package management

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
)

func readUsageV8(query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/queue?"+query, nil)
	(&Handler{}).GetUsageQueueV8(ctx)
	return rec
}

func TestUsageReplayHTTPEnvelopeAndLateCursor(t *testing.T) {
	withManagementUsageQueue(t, func() {
		start := redisqueue.ReadAfter(0, 1).NextSeq
		redisqueue.Enqueue([]byte("pruned"))
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(true)
		redisqueue.Enqueue([]byte(`{"id":1}`))
		redisqueue.Enqueue([]byte("usage-record"))
		query := "after_seq=" + strconv.FormatUint(start, 10) + "&count=10"
		first := readUsageV8(query)
		second := readUsageV8(query)
		if first.Code != 200 || first.Body.String() != second.Body.String() {
			t.Fatalf("replay not successful/repeatable: %d %s", first.Code, first.Body.String())
		}
		page := ojson.ParseBytes(first.Body.Bytes())
		if page.Get("oldest_seq").Uint()-start-1 != 1 || page.Get("next_seq").Uint() != start+3 || len(page.Get("records").Array()) != 2 {
			t.Fatalf("late cursor metadata: %s", first.Body.String())
		}
		boot := page.Get("boot_id").String()
		if boot == "" || page.Get("records.0.queue_seq").Uint() != start+2 || page.Get("records.0.payload.queue_seq").Uint() != start+2 || page.Get("records.0.payload.boot_id").String() != boot || page.Get("records.0.payload.id").Int() != 1 || page.Get("records.1.queue_seq").Uint() != start+3 || page.Get("records.1.payload").String() != "usage-record" {
			t.Fatalf("record envelopes: %s", first.Body.String())
		}
		if oldest := readUsageV8("after_seq=0&count=1"); ojson.GetBytes(oldest.Body.Bytes(), "records.0.queue_seq").Uint() != start+2 {
			t.Fatal("after_seq=0 did not return oldest held record")
		}
		if got := redisqueue.PopOldest(10); len(got) != 2 {
			t.Fatal("HTTP replay removed pop records")
		}
		empty := readUsageV8("after_seq=" + strconv.FormatUint(start+3, 10))
		if ojson.GetBytes(empty.Body.Bytes(), "records").Raw != "[]" || ojson.GetBytes(empty.Body.Bytes(), "next_seq").Uint() != start+3 {
			t.Fatalf("empty response: %s", empty.Body.String())
		}
	})
}

func TestUsageReplayHTTPRejectsMalformedCursorWithoutPopping(t *testing.T) {
	withManagementUsageQueue(t, func() {
		redisqueue.Enqueue([]byte("unchanged"))
		for _, value := range []string{"", "abc", "-1", "1.5", "18446744073709551616", "%20", "%ZZ"} {
			if rec := readUsageV8("after_seq=" + value); rec.Code != http.StatusBadRequest {
				t.Fatalf("after_seq=%q: status=%d", value, rec.Code)
			}
		}
		if got := redisqueue.PopOldest(10); len(got) != 1 || string(got[0]) != "unchanged" {
			t.Fatal("invalid cursor consumed pop record")
		}
	})
}

func TestUsageReplayHTTPCountClampDoesNotLimitPop(t *testing.T) {
	withManagementUsageQueue(t, func() {
		for range 1001 {
			redisqueue.Enqueue([]byte("record"))
		}
		for _, count := range []string{"1001", "9223372036854775807"} {
			rec := readUsageV8("after_seq=0&count=" + count)
			if rec.Code != 200 || len(ojson.GetBytes(rec.Body.Bytes(), "records").Array()) != 1000 {
				t.Fatalf("count=%s: status=%d", count, rec.Code)
			}
		}
		if rec := readUsageV8("count=1001"); rec.Code != 200 || len(ojson.ParseBytes(rec.Body.Bytes()).Array()) != 1001 {
			t.Fatal("replay clamp changed legacy pop count")
		}
	})
}
