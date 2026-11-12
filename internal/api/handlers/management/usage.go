package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
)

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

// GetUsageQueueV8 supports non-destructive after_seq reads. Replay count defaults
// to 1 and is clamped to 1000; the existing pop path keeps its count behavior.
func (h *Handler) GetUsageQueueV8(c *gin.Context) {
	query, errQuery := url.ParseQuery(c.Request.URL.RawQuery)
	if errQuery != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid query encoding"})
		return
	}
	values, replay := query["after_seq"]
	if !replay {
		h.GetUsageQueue(c)
		return
	}
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	after, errAfter := strconv.ParseUint(values[0], 10, 64)
	if errAfter != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "after_seq must be an unsigned integer"})
		return
	}
	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}
	if count > 1000 {
		count = 1000
	}
	page := redisqueue.ReadAfter(after, count)
	records := make([]gin.H, 0, len(page.Records))
	for _, record := range page.Records {
		records = append(records, gin.H{"queue_seq": record.QueueSeq, "payload": usageQueueRecord(record.Payload)})
	}
	c.JSON(http.StatusOK, gin.H{
		"boot_id": page.BootID, "oldest_seq": page.OldestSeq,
		"next_seq": page.NextSeq, "records": records,
	})
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}
